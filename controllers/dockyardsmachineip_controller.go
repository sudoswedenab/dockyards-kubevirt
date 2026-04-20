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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

const (
	ipamClaimAPIVersion = "k8s.cni.cncf.io/v1alpha1"
	ipamClaimKind       = "IPAMClaim"

	ipamStateConfigMapSuffix = "-external-node-ipam-state"
	ipamStateConfigMapKey    = "state.json"

	defaultExternalNodeInterface = "eth1"
	defaultControlPlaneReplicas  = 1

	ipamClaimNameSuffix = "-external-node-ip"
)

var (
	errNoWorkerIPsAvailable       = errors.New("no worker IPs available")
	errNoControlPlaneIPsAvailable = errors.New("no control plane IPs available")
)

// +kubebuilder:rbac:groups="",resources=configmaps,verbs=create;get;list;patch;update;watch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=talosconfigs,verbs=get;list;patch;update;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=delete;get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=ipamclaims,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=ipamclaims/status,verbs=get;patch;update

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

	machineKey := machine.Name
	isControlPlane := machineIsControlPlane(&machine)
	stateName := clusterKey.Name + ipamStateConfigMapSuffix

	if err := r.garbageCollectLeases(ctx, clusterKey.Namespace, clusterKey.Name, stateName, clusterRange); err != nil {
		return ctrl.Result{}, err
	}

	if !machine.DeletionTimestamp.IsZero() {
		if err := r.releaseMachineIP(ctx, clusterKey.Namespace, stateName, machineKey, clusterRange); err != nil {
			return ctrl.Result{}, err
		}

		if err := r.deleteIPAMClaim(ctx, machine.Namespace, machineIPAMClaimName(machine.Name)); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{}, nil
	}

	ip, err := r.allocateMachineIP(ctx, clusterKey.Namespace, stateName, machineKey, isControlPlane, clusterRange)
	if err != nil {
		if errors.Is(err, errNoControlPlaneIPsAvailable) || errors.Is(err, errNoWorkerIPsAvailable) {
			logger.Info("unable to allocate IP", "machine", machine.Name, "cluster", clusterKey.Name, "error", err)
			return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
		}

		return ctrl.Result{}, err
	}

	if err := r.ensureIPAMClaim(ctx, machine.Namespace, machineIPAMClaimName(machine.Name), clusterKey.Name, config.Interface, ip); err != nil {
		return ctrl.Result{}, err
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

	desiredAddress := fmt.Sprintf("%s/%d", ip, config.Subnet.Bits())
	updatedPatches, changed, err := upsertLinkConfigPatch(talosConfig.Spec.StrategicPatches, config.Interface, desiredAddress)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !changed {
		return ctrl.Result{}, nil
	}

	if talosConfig.Status.DataSecretName != nil {
		if err := r.reconcileLatePatchMachine(ctx, &machine, clusterKey.Name, isControlPlane); err != nil {
			return ctrl.Result{}, err
		}

		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	patch := client.MergeFrom(talosConfig.DeepCopy())
	talosConfig.Spec.StrategicPatches = updatedPatches

	if err := r.Patch(ctx, &talosConfig, patch); err != nil {
		return ctrl.Result{}, err
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

func (r *DockyardsMachineIPReconciler) ensureIPAMClaim(ctx context.Context, namespace, claimName, networkName, ifName string, ip netip.Addr) error {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1alpha1", Kind: ipamClaimKind})

	key := types.NamespacedName{Namespace: namespace, Name: claimName}
	err := r.Get(ctx, key, claim)
	if apierrors.IsNotFound(err) {
		claim.SetNamespace(namespace)
		claim.SetName(claimName)
		claim.Object["spec"] = map[string]any{
			"network":   networkName,
			"interface": ifName,
		}

		if err := r.Create(ctx, claim); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}

	statusIPs, found, err := unstructured.NestedStringSlice(claim.Object, "status", "ips")
	if err != nil {
		return err
	}

	if found && len(statusIPs) == 1 && statusIPs[0] == ip.String() {
		return nil
	}

	statusPatch := claim.DeepCopy()
	if err := unstructured.SetNestedStringSlice(statusPatch.Object, []string{ip.String()}, "status", "ips"); err != nil {
		return err
	}

	return r.Status().Patch(ctx, statusPatch, client.MergeFrom(claim))
}

func (r *DockyardsMachineIPReconciler) deleteIPAMClaim(ctx context.Context, namespace, claimName string) error {
	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(schema.GroupVersionKind{Group: "k8s.cni.cncf.io", Version: "v1alpha1", Kind: ipamClaimKind})

	err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claimName}, claim)
	if err != nil {
		return client.IgnoreNotFound(err)
	}

	return client.IgnoreNotFound(r.Delete(ctx, claim))
}

func (r *DockyardsMachineIPReconciler) allocateMachineIP(ctx context.Context, namespace, stateName, machineKey string, isControlPlane bool, subnet ipv4Range) (netip.Addr, error) {
	var allocated netip.Addr

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, state, err := r.getOrCreateStateConfigMap(ctx, namespace, stateName)
		if err != nil {
			return err
		}

		if lease, ok := state.Leases[machineKey]; ok {
			allocated = lease.IP
			return nil
		}

		ip, err := state.allocate(isControlPlane, subnet)
		if err != nil {
			return err
		}

		state.Leases[machineKey] = ipLease{IP: ip, ControlPlane: isControlPlane}
		allocated = ip

		if err := persistStateConfigMap(configMap, state); err != nil {
			return err
		}

		return r.Update(ctx, configMap)
	})
	if err != nil {
		return netip.Addr{}, err
	}

	return allocated, nil
}

func (r *DockyardsMachineIPReconciler) garbageCollectLeases(ctx context.Context, namespace, clusterName, stateName string, subnet ipv4Range) error {
	var machineList clusterv1.MachineList

	err := r.List(
		ctx,
		&machineList,
		client.InNamespace(namespace),
		client.MatchingLabels{clusterv1.ClusterNameLabel: clusterName},
	)
	if err != nil {
		return err
	}

	activeMachines := make(map[string]struct{}, len(machineList.Items))
	for i := range machineList.Items {
		activeMachines[machineList.Items[i].Name] = struct{}{}
	}

	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, state, err := r.getOrCreateStateConfigMap(ctx, namespace, stateName)
		if err != nil {
			return err
		}

		changed := false
		for machineName, lease := range state.Leases {
			if _, ok := activeMachines[machineName]; ok {
				continue
			}

			delete(state.Leases, machineName)
			state.release(lease, subnet)
			changed = true
		}

		if !changed {
			return nil
		}

		if err := persistStateConfigMap(configMap, state); err != nil {
			return err
		}

		return r.Update(ctx, configMap)
	})
}

func (r *DockyardsMachineIPReconciler) releaseMachineIP(ctx context.Context, namespace, stateName, machineKey string, subnet ipv4Range) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		configMap, state, err := r.getOrCreateStateConfigMap(ctx, namespace, stateName)
		if err != nil {
			return err
		}

		lease, ok := state.Leases[machineKey]
		if !ok {
			return nil
		}

		delete(state.Leases, machineKey)
		state.release(lease, subnet)

		if err := persistStateConfigMap(configMap, state); err != nil {
			return err
		}

		return r.Update(ctx, configMap)
	})
}

func (r *DockyardsMachineIPReconciler) getOrCreateStateConfigMap(ctx context.Context, namespace, name string) (*corev1.ConfigMap, *ipamState, error) {
	configMap := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: namespace, Name: name}

	err := r.Get(ctx, key, configMap)
	if apierrors.IsNotFound(err) {
		configMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: namespace,
				Name:      name,
			},
			Data: map[string]string{},
		}

		state := newIPAMState()
		if err := persistStateConfigMap(configMap, state); err != nil {
			return nil, nil, err
		}

		if err := r.Create(ctx, configMap); err != nil {
			if !apierrors.IsAlreadyExists(err) {
				return nil, nil, err
			}

			if err := r.Get(ctx, key, configMap); err != nil {
				return nil, nil, err
			}
		}
	} else if err != nil {
		return nil, nil, err
	}

	state, err := parseIPAMState(configMap)
	if err != nil {
		return nil, nil, err
	}

	return configMap, state, nil
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

type ipLease struct {
	IP           netip.Addr `json:"ip"`
	ControlPlane bool       `json:"controlPlane"`
}

type ipamState struct {
	Leases               map[string]ipLease `json:"leases"`
	ReleasedControlPlane []netip.Addr       `json:"releasedControlPlane"`
	ReleasedWorker       []netip.Addr       `json:"releasedWorker"`
}

func newIPAMState() *ipamState {
	return &ipamState{Leases: map[string]ipLease{}}
}

func parseIPAMState(configMap *corev1.ConfigMap) (*ipamState, error) {
	if configMap.Data == nil {
		return newIPAMState(), nil
	}

	raw, ok := configMap.Data[ipamStateConfigMapKey]
	if !ok || raw == "" {
		return newIPAMState(), nil
	}

	var state ipamState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, err
	}

	if state.Leases == nil {
		state.Leases = map[string]ipLease{}
	}

	return &state, nil
}

func persistStateConfigMap(configMap *corev1.ConfigMap, state *ipamState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}

	if configMap.Data == nil {
		configMap.Data = map[string]string{}
	}

	configMap.Data[ipamStateConfigMapKey] = string(raw)

	return nil
}

func (s *ipamState) allocate(controlPlane bool, subnet ipv4Range) (netip.Addr, error) {
	inUse := make(map[uint32]struct{}, len(s.Leases))
	for _, lease := range s.Leases {
		inUse[ipv4ToUint32(lease.IP)] = struct{}{}
	}

	if controlPlane {
		addr, queue, ok := popReusableIP(s.ReleasedControlPlane, inUse, subnet.controlPlaneStart, subnet.controlPlaneEnd)
		s.ReleasedControlPlane = queue
		if ok {
			return addr, nil
		}

		for v := subnet.controlPlaneStart; v <= subnet.controlPlaneEnd; v++ {
			if _, exists := inUse[v]; exists {
				continue
			}

			return uint32ToIPv4(v), nil
		}

		return netip.Addr{}, errNoControlPlaneIPsAvailable
	}

	addr, queue, ok := popReusableIP(s.ReleasedWorker, inUse, subnet.workerStart, subnet.workerEnd)
	s.ReleasedWorker = queue
	if ok {
		return addr, nil
	}

	for v := subnet.workerStart; v <= subnet.workerEnd; v++ {
		if _, exists := inUse[v]; exists {
			continue
		}

		return uint32ToIPv4(v), nil
	}

	return netip.Addr{}, errNoWorkerIPsAvailable
}

func (s *ipamState) release(lease ipLease, subnet ipv4Range) {
	value := ipv4ToUint32(lease.IP)

	if lease.ControlPlane {
		if value < subnet.controlPlaneStart || value > subnet.controlPlaneEnd {
			return
		}

		if slices.Contains(s.ReleasedControlPlane, lease.IP) {
			return
		}

		s.ReleasedControlPlane = append(s.ReleasedControlPlane, lease.IP)

		return
	}

	if value < subnet.workerStart || value > subnet.workerEnd {
		return
	}

	if slices.Contains(s.ReleasedWorker, lease.IP) {
		return
	}

	s.ReleasedWorker = append(s.ReleasedWorker, lease.IP)
}

func popReusableIP(queue []netip.Addr, inUse map[uint32]struct{}, start, end uint32) (netip.Addr, []netip.Addr, bool) {
	for i, addr := range queue {
		value := ipv4ToUint32(addr)
		if value < start || value > end {
			continue
		}

		if _, exists := inUse[value]; exists {
			continue
		}

		remaining := make([]netip.Addr, 0, len(queue)-1)
		remaining = append(remaining, queue[:i]...)
		remaining = append(remaining, queue[i+1:]...)

		return addr, remaining, true
	}

	return netip.Addr{}, queue, false
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
