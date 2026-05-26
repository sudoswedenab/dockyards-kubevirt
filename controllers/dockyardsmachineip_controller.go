// Copyright 2026 Sudo Sweden AB
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"slices"
	"time"

	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	dockyardskubevirtv1 "github.com/sudoswedenab/dockyards-kubevirt/api/v1alpha1"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	defaultExternalNodeInterface = "eth1"
	defaultControlPlaneReplicas  = 1

	ipamClaimNameSuffix = "-external-node-ip"

	ipamClaimPhasePending = "Pending"
	ipamClaimPhaseReady   = "Ready"
	ipamClaimPhaseFailed  = "Failed"
)

var (
	errNoWorkerIPsAvailable       = errors.New("no worker IPs available")
	errNoControlPlaneIPsAvailable = errors.New("no control plane IPs available")
)

// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;patch;update;watch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=talosconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=delete;get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=kubevirt.dockyards.io,resources=ipamclaims,verbs=create;get;list;patch;update;watch
// +kubebuilder:rbac:groups=kubevirt.dockyards.io,resources=ipamclaims/status,verbs=get;patch;update

type DockyardsMachineIPReconciler struct {
	client.Client
}

func (r *DockyardsMachineIPReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	var machine clusterv1.Machine
	if err := r.Get(ctx, req.NamespacedName, &machine); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if machine.Spec.ClusterName == "" {
		return ctrl.Result{}, nil
	}

	clusterKey := types.NamespacedName{Namespace: machine.Namespace, Name: machine.Spec.ClusterName}

	var dockyardsCluster dockyardsv1.Cluster
	if err := r.Get(ctx, clusterKey, &dockyardsCluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	config, err := r.getExternalNodeConfig(ctx, clusterKey)
	if err != nil {
		return ctrl.Result{}, err
	}
	if config == nil {
		return ctrl.Result{}, nil
	}

	controlPlaneReplicas, err := r.desiredControlPlaneReplicas(ctx, clusterKey)
	if err != nil {
		return ctrl.Result{}, err
	}

	clusterRange, err := newIPv4Range(config.Subnet, controlPlaneReplicas+1)
	if err != nil {
		return ctrl.Result{}, err
	}

	isControlPlane := machineIsControlPlane(&machine)

	if !machine.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	claim, err := r.ensureDockyardsIPAMClaim(ctx, &machine, clusterKey.Name, config.Interface, config.Subnet, isControlPlane)
	if err != nil {
		return ctrl.Result{}, err
	}

	if claim.Spec.Address == "" {
		if statusErr := r.setClaimStatus(ctx, claim, ipamClaimPhasePending, "AllocatingAddress", "waiting for address allocation"); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
	}

	ip, err := r.ensureClaimAddress(ctx, claim, clusterRange)
	if err != nil {
		if statusErr := r.setClaimStatus(ctx, claim, ipamClaimPhaseFailed, "AddressAllocationFailed", err.Error()); statusErr != nil {
			return ctrl.Result{}, statusErr
		}

		if errors.Is(err, errNoControlPlaneIPsAvailable) || errors.Is(err, errNoWorkerIPsAvailable) {
			logger.Info("unable to allocate IP", "machine", machine.Name, "cluster", clusterKey.Name, "error", err)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		return ctrl.Result{}, err
	}

	if statusErr := r.setClaimStatus(
		ctx,
		claim,
		ipamClaimPhaseReady,
		"AddressAllocated",
		fmt.Sprintf("allocated address %s", ip),
	); statusErr != nil {
		return ctrl.Result{}, statusErr
	}

	talosConfigRef := machine.Spec.Bootstrap.ConfigRef
	if talosConfigRef == nil || talosConfigRef.Kind != "TalosConfig" {
		return ctrl.Result{}, nil
	}

	var talosConfig bootstrapv1.TalosConfig
	talosConfigKey := types.NamespacedName{Namespace: machine.Namespace, Name: talosConfigRef.Name}
	if err := r.Get(ctx, talosConfigKey, &talosConfig); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if talosConfig.Status.DataSecretName == nil {
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	bootstrapDataSecret := &corev1.Secret{}
	bootstrapDataSecretKey := types.NamespacedName{Namespace: machine.Namespace, Name: *talosConfig.Status.DataSecretName}
	if err := r.Get(ctx, bootstrapDataSecretKey, bootstrapDataSecret); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	bootstrapData, ok := bootstrapDataSecret.Data["value"]
	if !ok {
		return ctrl.Result{}, nil
	}

	desiredAddress := fmt.Sprintf("%s/%d", ip, config.Subnet.Bits())
	updatedBootstrapData, changed, err := upsertLinkConfigInBootstrapData(bootstrapData, config.Interface, desiredAddress)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !changed {
		return ctrl.Result{}, nil
	}

	secretPatch := client.MergeFrom(bootstrapDataSecret.DeepCopy())
	bootstrapDataSecret.Data["value"] = updatedBootstrapData

	if err := r.Patch(ctx, bootstrapDataSecret, secretPatch); err != nil {
		return ctrl.Result{}, err
	}

	if machine.Status.NodeRef != nil {
		if err := r.reconcileLatePatchMachine(ctx, &machine, clusterKey.Name, isControlPlane); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsMachineIPReconciler) reconcileLatePatchMachine(ctx context.Context, machine *clusterv1.Machine, clusterName string, isControlPlane bool) error {
	deletingRoleMachine, err := r.hasDeletingMachineInRole(ctx, machine.Namespace, clusterName, isControlPlane, machine.Name)
	if err != nil {
		return err
	}

	if deletingRoleMachine {
		return nil
	}

	return r.Delete(ctx, machine)
}

func (r *DockyardsMachineIPReconciler) hasDeletingMachineInRole(ctx context.Context, namespace, clusterName string, isControlPlane bool, exceptName string) (bool, error) {
	var machineList clusterv1.MachineList

	err := r.List(
		ctx,
		&machineList,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
	)
	if err != nil {
		return false, err
	}

	for i := range machineList.Items {
		m := &machineList.Items[i]
		if m.Name == exceptName {
			continue
		}

		if m.DeletionTimestamp.IsZero() {
			continue
		}

		if machineIsControlPlane(m) == isControlPlane {
			return true, nil
		}
	}

	return false, nil
}

func (r *DockyardsMachineIPReconciler) getExternalNodeConfig(ctx context.Context, clusterKey types.NamespacedName) (*externalNodeConfig, error) {
	unstructuredCluster := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": dockyardsv1.GroupVersion.String(),
		"kind":       dockyardsv1.ClusterKind,
		"metadata": map[string]any{
			"name":      clusterKey.Name,
			"namespace": clusterKey.Namespace,
		},
	}}

	if err := r.Get(ctx, clusterKey, &unstructuredCluster); err != nil {
		return nil, err
	}

	subnetRaw, found, err := unstructured.NestedString(unstructuredCluster.Object, "spec", "advanced", "kubevirt", "talos", "externalNodeIPv4Subnet")
	if err != nil {
		return nil, err
	}
	if !found || subnetRaw == "" {
		return nil, nil
	}

	subnet, err := netip.ParsePrefix(subnetRaw)
	if err != nil {
		return nil, fmt.Errorf("invalid externalNodeIPv4Subnet %q: %w", subnetRaw, err)
	}

	if !subnet.Addr().Is4() {
		return nil, fmt.Errorf("externalNodeIPv4Subnet %q is not IPv4", subnetRaw)
	}

	interfaceName, found, err := unstructured.NestedString(unstructuredCluster.Object, "spec", "advanced", "kubevirt", "talos", "externalNodeInterface")
	if err != nil {
		return nil, err
	}

	if !found || interfaceName == "" {
		interfaceName = defaultExternalNodeInterface
	}

	return &externalNodeConfig{Subnet: subnet.Masked(), Interface: interfaceName}, nil
}

func (r *DockyardsMachineIPReconciler) desiredControlPlaneReplicas(ctx context.Context, clusterKey types.NamespacedName) (int, error) {
	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, clusterKey, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return defaultControlPlaneReplicas, nil
		}

		return 0, err
	}

	if cluster.Spec.ControlPlaneRef == nil {
		return defaultControlPlaneReplicas, nil
	}

	if cluster.Spec.ControlPlaneRef.Kind != "TalosControlPlane" {
		return defaultControlPlaneReplicas, nil
	}

	controlPlaneNamespace := cluster.Namespace
	if cluster.Spec.ControlPlaneRef.Namespace != "" {
		controlPlaneNamespace = cluster.Spec.ControlPlaneRef.Namespace
	}

	controlPlane := &controlplanev1.TalosControlPlane{}
	controlPlaneKey := types.NamespacedName{Name: cluster.Spec.ControlPlaneRef.Name, Namespace: controlPlaneNamespace}

	if err := r.Get(ctx, controlPlaneKey, controlPlane); err != nil {
		if apierrors.IsNotFound(err) {
			return defaultControlPlaneReplicas, nil
		}

		return 0, err
	}

	replicas := int(controlPlane.Spec.GetReplicas())
	if replicas < 1 {
		return defaultControlPlaneReplicas, nil
	}

	return replicas, nil
}

func (r *DockyardsMachineIPReconciler) ensureDockyardsIPAMClaim(
	ctx context.Context,
	machine *clusterv1.Machine,
	clusterName,
	ifName string,
	subnet netip.Prefix,
	controlPlane bool,
) (*dockyardskubevirtv1.DockyardsIPAMClaim, error) {
	claimKey := types.NamespacedName{Namespace: machine.Namespace, Name: machineIPAMClaimName(machine.Name)}
	claim := &dockyardskubevirtv1.DockyardsIPAMClaim{}
	err := r.Get(ctx, claimKey, claim)
	if apierrors.IsNotFound(err) {
		claim = &dockyardskubevirtv1.DockyardsIPAMClaim{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: machine.Namespace,
				Name:      machineIPAMClaimName(machine.Name),
				OwnerReferences: []metav1.OwnerReference{
					machineOwnerReference(machine),
				},
			},
			Spec: dockyardskubevirtv1.DockyardsIPAMClaimSpec{
				ClusterName:  clusterName,
				MachineName:  machine.Name,
				Interface:    ifName,
				Subnet:       subnet.String(),
				ControlPlane: controlPlane,
			},
		}

		if err := r.Create(ctx, claim); err != nil {
			return nil, err
		}

		return claim, nil
	}

	if err != nil {
		return nil, err
	}

	patch := client.MergeFrom(claim.DeepCopy())
	changed := false

	if claim.Spec.ClusterName != clusterName {
		claim.Spec.ClusterName = clusterName
		changed = true
	}

	if claim.Spec.MachineName != machine.Name {
		claim.Spec.MachineName = machine.Name
		changed = true
	}

	if claim.Spec.Interface != ifName {
		claim.Spec.Interface = ifName
		changed = true
	}

	if claim.Spec.Subnet != subnet.String() {
		claim.Spec.Subnet = subnet.String()
		changed = true
	}

	if claim.Spec.ControlPlane != controlPlane {
		claim.Spec.ControlPlane = controlPlane
		changed = true
	}

	if !machineOwnerReferenceExists(claim.OwnerReferences, machine.UID) {
		claim.OwnerReferences = []metav1.OwnerReference{machineOwnerReference(machine)}
		changed = true
	}

	if changed {
		if err := r.Patch(ctx, claim, patch); err != nil {
			return nil, err
		}
	}

	return claim, nil
}

func (r *DockyardsMachineIPReconciler) ensureClaimAddress(ctx context.Context, claim *dockyardskubevirtv1.DockyardsIPAMClaim, subnet ipv4Range) (netip.Addr, error) {
	if claim.Spec.Address != "" {
		ip, err := netip.ParseAddr(claim.Spec.Address)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("invalid dockyards ipam claim address %q: %w", claim.Spec.Address, err)
		}

		if !ip.Is4() {
			return netip.Addr{}, fmt.Errorf("invalid non-ipv4 address in dockyards ipam claim: %q", claim.Spec.Address)
		}

		return ip, nil
	}

	ip, err := r.allocateClaimAddress(ctx, claim.Namespace, claim.Name, claim.Spec.ClusterName, claim.Spec.ControlPlane, subnet)
	if err != nil {
		return netip.Addr{}, err
	}

	patch := client.MergeFrom(claim.DeepCopy())
	claim.Spec.Address = ip.String()

	if err := r.Patch(ctx, claim, patch); err != nil {
		return netip.Addr{}, err
	}

	return ip, nil
}

func (r *DockyardsMachineIPReconciler) setClaimStatus(
	ctx context.Context,
	claim *dockyardskubevirtv1.DockyardsIPAMClaim,
	phase,
	reason,
	message string,
) error {
	patch := client.MergeFrom(claim.DeepCopy())

	if claim.Status.Phase == phase &&
		claim.Status.Reason == reason &&
		claim.Status.Message == message &&
		claim.Status.ObservedGeneration == claim.Generation {
		return nil
	}

	claim.Status.Phase = phase
	claim.Status.Reason = reason
	claim.Status.Message = message
	claim.Status.ObservedGeneration = claim.Generation

	return r.Status().Patch(ctx, claim, patch)
}

func (r *DockyardsMachineIPReconciler) allocateClaimAddress(
	ctx context.Context,
	namespace,
	exceptClaimName,
	clusterName string,
	controlPlane bool,
	subnet ipv4Range,
) (netip.Addr, error) {
	var claimList dockyardskubevirtv1.DockyardsIPAMClaimList
	err := r.List(ctx, &claimList, client.InNamespace(namespace))
	if err != nil {
		return netip.Addr{}, err
	}

	inUse := map[uint32]struct{}{}
	for _, claim := range claimList.Items {
		if claim.Name == exceptClaimName {
			continue
		}

		if claim.Spec.ClusterName != clusterName || claim.Spec.Address == "" {
			continue
		}

		ip, err := netip.ParseAddr(claim.Spec.Address)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("invalid address %q in dockyards ipam claim %s/%s: %w", claim.Spec.Address, claim.Namespace, claim.Name, err)
		}

		if !ip.Is4() {
			return netip.Addr{}, fmt.Errorf("non-ipv4 address %q in dockyards ipam claim %s/%s", claim.Spec.Address, claim.Namespace, claim.Name)
		}

		inUse[ipv4ToUint32(ip)] = struct{}{}
	}

	return nextAvailableIP(controlPlane, inUse, subnet)
}

func nextAvailableIP(controlPlane bool, inUse map[uint32]struct{}, subnet ipv4Range) (netip.Addr, error) {
	if controlPlane {
		for v := subnet.controlPlaneStart; v <= subnet.controlPlaneEnd; v++ {
			if _, exists := inUse[v]; exists {
				continue
			}

			return uint32ToIPv4(v), nil
		}

		return netip.Addr{}, errNoControlPlaneIPsAvailable
	}

	for v := subnet.workerStart; v <= subnet.workerEnd; v++ {
		if _, exists := inUse[v]; exists {
			continue
		}

		return uint32ToIPv4(v), nil
	}

	return netip.Addr{}, errNoWorkerIPsAvailable
}

func (r *DockyardsMachineIPReconciler) dockyardsClusterToMachines(ctx context.Context, obj client.Object) []ctrl.Request {
	cluster, ok := obj.(*dockyardsv1.Cluster)
	if !ok {
		return nil
	}

	var machineList clusterv1.MachineList
	err := r.List(
		ctx,
		&machineList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: cluster.Name},
	)
	if err != nil {
		return nil
	}

	requests := make([]ctrl.Request, 0, len(machineList.Items))
	for i := range machineList.Items {
		machine := &machineList.Items[i]
		requests = append(requests, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
	}

	return requests
}

func (r *DockyardsMachineIPReconciler) SetupWithManager(m ctrl.Manager) error {
	scheme := m.GetScheme()

	_ = bootstrapv1.AddToScheme(scheme)
	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)
	_ = dockyardskubevirtv1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)

	return ctrl.NewControllerManagedBy(m).
		For(&clusterv1.Machine{}).
		Watches(
			&dockyardsv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.dockyardsClusterToMachines),
		).
		Complete(r)
}

func machineIsControlPlane(machine *clusterv1.Machine) bool {
	_, ok := machine.Labels[clusterv1.MachineControlPlaneLabel]

	return ok
}

func machineIPAMClaimName(machineName string) string {
	return machineName + ipamClaimNameSuffix
}

type externalNodeConfig struct {
	Subnet    netip.Prefix
	Interface string
}

func machineOwnerReference(machine *clusterv1.Machine) metav1.OwnerReference {
	controller := false
	blockOwnerDeletion := false

	return metav1.OwnerReference{
		APIVersion:         clusterv1.GroupVersion.String(),
		Kind:               "Machine",
		Name:               machine.Name,
		UID:                machine.UID,
		Controller:         &controller,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}
}

func machineOwnerReferenceExists(ownerReferences []metav1.OwnerReference, uid types.UID) bool {
	for _, ownerReference := range ownerReferences {
		if ownerReference.UID == uid {
			return true
		}
	}

	return false
}

type ipv4Range struct {
	network           uint32
	broadcast         uint32
	controlPlaneStart uint32
	controlPlaneEnd   uint32
	workerStart       uint32
	workerEnd         uint32
}

func newIPv4Range(prefix netip.Prefix, controlPlaneReserved int) (ipv4Range, error) {
	if controlPlaneReserved < 1 {
		controlPlaneReserved = 1
	}

	masked := prefix.Masked()
	if !masked.Addr().Is4() {
		return ipv4Range{}, fmt.Errorf("subnet %q is not IPv4", prefix)
	}

	bits := masked.Bits()
	if bits > 32 {
		return ipv4Range{}, fmt.Errorf("invalid subnet mask bits: %d", bits)
	}

	network := ipv4ToUint32(masked.Addr())
	hostBits := 32 - bits
	if hostBits == 0 {
		return ipv4Range{}, fmt.Errorf("subnet %q does not contain host addresses", prefix)
	}

	broadcast := network + (1 << hostBits) - 1

	firstUsable := network + 1
	controlPlaneStart := firstUsable + 1
	controlPlaneEnd := controlPlaneStart + uint32(controlPlaneReserved) - 1
	workerStart := controlPlaneEnd + 1
	workerEnd := broadcast - 1

	if workerStart > workerEnd {
		return ipv4Range{}, fmt.Errorf("subnet %q too small for reserved gateway and control-plane addresses", prefix)
	}

	return ipv4Range{
		network:           network,
		broadcast:         broadcast,
		controlPlaneStart: controlPlaneStart,
		controlPlaneEnd:   controlPlaneEnd,
		workerStart:       workerStart,
		workerEnd:         workerEnd,
	}, nil
}

func ipv4ToUint32(addr netip.Addr) uint32 {
	bytes := addr.As4()

	return uint32(bytes[0])<<24 | uint32(bytes[1])<<16 | uint32(bytes[2])<<8 | uint32(bytes[3])
}

func uint32ToIPv4(value uint32) netip.Addr {
	bytes := [4]byte{
		byte(value >> 24),
		byte(value >> 16),
		byte(value >> 8),
		byte(value),
	}

	return netip.AddrFrom4(bytes)
}

func upsertLinkConfigPatch(patches []string, interfaceName, addressWithPrefix string) ([]string, bool, error) {
	updated := slices.Clone(patches)
	for i := range updated {
		document := map[string]any{}
		if err := yaml.Unmarshal([]byte(updated[i]), &document); err != nil {
			continue
		}

		kind, _, err := unstructured.NestedString(document, "kind")
		if err != nil || kind != "LinkConfig" {
			continue
		}

		name, _, err := unstructured.NestedString(document, "name")
		if err != nil || name != interfaceName {
			continue
		}

		updatedDoc, changed, err := setLinkConfigAddress(document, interfaceName, addressWithPrefix)
		if err != nil {
			return nil, false, err
		}

		if !changed {
			return patches, false, nil
		}

		raw, err := yaml.Marshal(updatedDoc)
		if err != nil {
			return nil, false, err
		}

		updated[i] = string(raw)

		return updated, true, nil
	}

	newDoc := map[string]any{}
	updatedDoc, _, err := setLinkConfigAddress(newDoc, interfaceName, addressWithPrefix)
	if err != nil {
		return nil, false, err
	}

	raw, err := yaml.Marshal(updatedDoc)
	if err != nil {
		return nil, false, err
	}

	updated = append(updated, string(raw))

	return updated, true, nil
}

func upsertLinkConfigInBootstrapData(data []byte, interfaceName, addressWithPrefix string) ([]byte, bool, error) {
	documents, err := decodeYAMLDocuments(data)
	if err != nil {
		return nil, false, err
	}

	stringDocs := make([]string, 0, len(documents))
	for _, doc := range documents {
		raw, err := yaml.Marshal(doc)
		if err != nil {
			return nil, false, err
		}

		stringDocs = append(stringDocs, string(raw))
	}

	updatedDocs, changed, err := upsertLinkConfigPatch(stringDocs, interfaceName, addressWithPrefix)
	if err != nil {
		return nil, false, err
	}

	if !changed {
		return data, false, nil
	}

	updatedData, err := encodeYAMLDocuments(updatedDocs)
	if err != nil {
		return nil, false, err
	}

	return updatedData, true, nil
}

func decodeYAMLDocuments(data []byte) ([]map[string]any, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	result := []map[string]any{}

	for {
		document := map[string]any{}
		err := decoder.Decode(&document)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}

		if len(document) == 0 {
			continue
		}

		result = append(result, document)
	}

	return result, nil
}

func encodeYAMLDocuments(documents []string) ([]byte, error) {
	buffer := bytes.NewBuffer(nil)
	encoder := yaml.NewEncoder(buffer)

	for _, document := range documents {
		decoded := map[string]any{}
		if err := yaml.Unmarshal([]byte(document), &decoded); err != nil {
			return nil, err
		}

		if err := encoder.Encode(decoded); err != nil {
			return nil, err
		}
	}

	if err := encoder.Close(); err != nil {
		return nil, err
	}

	return buffer.Bytes(), nil
}

func setLinkConfigAddress(doc map[string]any, interfaceName, addressWithPrefix string) (map[string]any, bool, error) {
	desiredPrefix, err := netip.ParsePrefix(addressWithPrefix)
	if err != nil {
		return nil, false, err
	}

	addresses, found, err := unstructured.NestedSlice(doc, "addresses")
	if err != nil {
		return nil, false, err
	}
	if !found {
		addresses = nil
	}

	hadUp := false
	up := true
	if value, upFound, err := unstructured.NestedBool(doc, "up"); err == nil && upFound {
		hadUp = true
		up = value
	} else if err != nil {
		return nil, false, err
	}

	changed := false
	managedSubnet := desiredPrefix.Masked()

	nextAddresses := make([]any, 0, len(addresses)+1)
	foundDesired := false

	for _, entry := range addresses {
		addrMap, ok := entry.(map[string]any)
		if !ok {
			nextAddresses = append(nextAddresses, entry)
			continue
		}

		valueRaw, ok := addrMap["address"]
		if !ok {
			nextAddresses = append(nextAddresses, entry)
			continue
		}

		value, ok := valueRaw.(string)
		if !ok {
			nextAddresses = append(nextAddresses, entry)
			continue
		}

		existingPrefix, err := netip.ParsePrefix(value)
		if err != nil {
			nextAddresses = append(nextAddresses, entry)
			continue
		}

		if existingPrefix == desiredPrefix {
			foundDesired = true
			nextAddresses = append(nextAddresses, entry)
			continue
		}

		if existingPrefix.Bits() == desiredPrefix.Bits() && managedSubnet.Contains(existingPrefix.Addr()) {
			changed = true
			continue
		}

		nextAddresses = append(nextAddresses, entry)
	}

	if !foundDesired {
		nextAddresses = append(nextAddresses, map[string]any{"address": addressWithPrefix})
		changed = true
	}

	if !hadUp {
		up = true
		changed = true
	}

	if apiVersion, found, _ := unstructured.NestedString(doc, "apiVersion"); !found || apiVersion != "v1alpha1" {
		changed = true
	}

	if kind, found, _ := unstructured.NestedString(doc, "kind"); !found || kind != "LinkConfig" {
		changed = true
	}

	if name, found, _ := unstructured.NestedString(doc, "name"); !found || name != interfaceName {
		changed = true
	}

	if err := unstructured.SetNestedField(doc, "v1alpha1", "apiVersion"); err != nil {
		return nil, false, err
	}

	if err := unstructured.SetNestedField(doc, "LinkConfig", "kind"); err != nil {
		return nil, false, err
	}

	if err := unstructured.SetNestedField(doc, interfaceName, "name"); err != nil {
		return nil, false, err
	}

	if err := unstructured.SetNestedField(doc, up, "up"); err != nil {
		return nil, false, err
	}

	if err := unstructured.SetNestedSlice(doc, nextAddresses, "addresses"); err != nil {
		return nil, false, err
	}

	return doc, changed, nil
}
