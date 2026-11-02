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
	"errors"
	"net/netip"
	"reflect"
	"strings"
	"testing"
	"time"

	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	machineapi "github.com/siderolabs/talos/pkg/machinery/api/machine"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	dockyardskubevirtv1 "github.com/sudoswedenab/dockyards-kubevirt/api/v1alpha1"
	"google.golang.org/grpc"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func TestNewIPv4Range(t *testing.T) {
	t.Parallel()

	prefix := netip.MustParsePrefix("10.71.22.160/27")
	rangeConfig, err := newIPv4Range(prefix, 4)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if got, want := uint32ToIPv4(rangeConfig.controlPlaneStart).String(), "10.71.22.162"; got != want {
		t.Fatalf("unexpected controlPlaneStart: got %q, want %q", got, want)
	}

	if got, want := uint32ToIPv4(rangeConfig.controlPlaneEnd).String(), "10.71.22.165"; got != want {
		t.Fatalf("unexpected controlPlaneEnd: got %q, want %q", got, want)
	}

	if got, want := uint32ToIPv4(rangeConfig.workerStart).String(), "10.71.22.166"; got != want {
		t.Fatalf("unexpected workerStart: got %q, want %q", got, want)
	}

	if got, want := uint32ToIPv4(rangeConfig.workerEnd).String(), "10.71.22.190"; got != want {
		t.Fatalf("unexpected workerEnd: got %q, want %q", got, want)
	}
}

func TestNextAvailableIPRoleSeparation(t *testing.T) {
	t.Parallel()

	rangeConfig, err := newIPv4Range(netip.MustParsePrefix("10.71.22.160/27"), 4)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	inUse := map[uint32]struct{}{}

	cp1, err := nextAvailableIP(true, inUse, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got, want := cp1.String(), "10.71.22.162"; got != want {
		t.Fatalf("unexpected first cp ip: got %q, want %q", got, want)
	}

	inUse[ipv4ToUint32(cp1)] = struct{}{}

	worker1, err := nextAvailableIP(false, inUse, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got, want := worker1.String(), "10.71.22.166"; got != want {
		t.Fatalf("unexpected first worker ip: got %q, want %q", got, want)
	}

	inUse[ipv4ToUint32(worker1)] = struct{}{}
	worker2, err := nextAvailableIP(false, inUse, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got, want := worker2.String(), "10.71.22.167"; got != want {
		t.Fatalf("expected next free worker ip, got %q want %q", got, want)
	}
}

func TestNextAvailableIPReusesAddressWhenClaimDisappears(t *testing.T) {
	t.Parallel()

	rangeConfig, err := newIPv4Range(netip.MustParsePrefix("10.71.22.160/27"), 4)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	inUse := map[uint32]struct{}{}

	worker1, err := nextAvailableIP(false, inUse, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	inUse[ipv4ToUint32(worker1)] = struct{}{}
	worker2, err := nextAvailableIP(false, inUse, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if got, want := worker2.String(), "10.71.22.167"; got != want {
		t.Fatalf("unexpected second worker ip: got %q, want %q", got, want)
	}

	delete(inUse, ipv4ToUint32(worker1))

	reused, err := nextAvailableIP(false, inUse, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if got, want := reused.String(), worker1.String(); got != want {
		t.Fatalf("expected disappeared claim ip to be reused, got %q want %q", got, want)
	}
}

func TestControlPlaneReserveExhaustion(t *testing.T) {
	t.Parallel()

	const controlPlaneReserved = 4

	rangeConfig, err := newIPv4Range(netip.MustParsePrefix("10.71.22.160/27"), controlPlaneReserved)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	inUse := map[uint32]struct{}{}

	for i := 0; i < controlPlaneReserved; i++ {
		ip, allocErr := nextAvailableIP(true, inUse, rangeConfig)
		if allocErr != nil {
			t.Fatalf("expected allocation to succeed on slot %d: %v", i, allocErr)
		}

		inUse[ipv4ToUint32(ip)] = struct{}{}
	}

	_, err = nextAvailableIP(true, inUse, rangeConfig)
	if !errors.Is(err, errNoControlPlaneIPsAvailable) {
		t.Fatalf("expected errNoControlPlaneIPsAvailable, got %v", err)
	}
}

func TestNewIPv4RangeHonorsDynamicControlPlaneReserve(t *testing.T) {
	t.Parallel()

	rangeConfig, err := newIPv4Range(netip.MustParsePrefix("10.71.22.160/27"), 2)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if got, want := uint32ToIPv4(rangeConfig.controlPlaneEnd).String(), "10.71.22.163"; got != want {
		t.Fatalf("unexpected controlPlaneEnd for reserve=2: got %q, want %q", got, want)
	}

	if got, want := uint32ToIPv4(rangeConfig.workerStart).String(), "10.71.22.164"; got != want {
		t.Fatalf("unexpected workerStart for reserve=2: got %q, want %q", got, want)
	}
}

func TestUpsertLinkConfigPatch(t *testing.T) {
	t.Parallel()

	patches := []string{}
	result, changed, err := upsertLinkConfigPatch(patches, "eth1", "10.71.22.171/27")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !changed {
		t.Fatal("expected patch list to change")
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(result))
	}

	_, changed, err = upsertLinkConfigPatch(result, "eth1", "10.71.22.171/27")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if changed {
		t.Fatal("expected idempotent upsert to not change")
	}
}

func TestUpsertLinkConfigPatchPreservesRoutes(t *testing.T) {
	t.Parallel()

	patches := []string{`apiVersion: v1alpha1
kind: LinkConfig
name: eth1
routes:
  - destination: 0.0.0.0/0
    gateway: 10.71.22.161
addresses:
  - address: 10.71.22.172/27
`}

	result, changed, err := upsertLinkConfigPatch(patches, "eth1", "10.71.22.173/27")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !changed {
		t.Fatal("expected patch list to change")
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 patch, got %d", len(result))
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(result[0]), &doc); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	routes, found, err := unstructured.NestedSlice(doc, "routes")
	if err != nil || !found || len(routes) != 1 {
		t.Fatalf("expected routes to be preserved")
	}

	addressEntries, found, err := unstructured.NestedSlice(doc, "addresses")
	if err != nil || !found || len(addressEntries) != 1 {
		t.Fatalf("expected exactly one address")
	}

	addr := addressEntries[0].(map[string]any)
	if got, ok := addr["address"].(string); !ok || got != "10.71.22.173/27" {
		t.Fatalf("unexpected updated address: %v", addr["address"])
	}
}

func TestUpsertLinkConfigPatchPreservesUnmanagedAddresses(t *testing.T) {
	t.Parallel()

	patches := []string{`apiVersion: v1alpha1
kind: LinkConfig
name: eth1
addresses:
  - address: 10.71.22.172/27
  - address: 192.168.10.5/24
`}

	result, changed, err := upsertLinkConfigPatch(patches, "eth1", "10.71.22.173/27")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if !changed {
		t.Fatal("expected patch list to change")
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(result[0]), &doc); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	addressEntries, found, err := unstructured.NestedSlice(doc, "addresses")
	if err != nil || !found || len(addressEntries) != 2 {
		t.Fatalf("expected exactly two addresses")
	}

	actual := map[string]bool{}
	for _, entry := range addressEntries {
		addr := entry.(map[string]any)
		actual[addr["address"].(string)] = true
	}

	if !actual["10.71.22.173/27"] {
		t.Fatalf("expected managed address to be updated")
	}

	if !actual["192.168.10.5/24"] {
		t.Fatalf("expected unmanaged address to be preserved")
	}
}

func TestUpsertLinkConfigPatchPreservesExplicitUp(t *testing.T) {
	t.Parallel()

	patches := []string{`apiVersion: v1alpha1
kind: LinkConfig
name: eth1
up: false
addresses:
  - address: 10.71.22.172/27
`}

	result, _, err := upsertLinkConfigPatch(patches, "eth1", "10.71.22.173/27")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(result[0]), &doc); err != nil {
		t.Fatalf("failed to unmarshal patch: %v", err)
	}

	up, found, err := unstructured.NestedBool(doc, "up")
	if err != nil || !found {
		t.Fatalf("expected up value")
	}

	if up {
		t.Fatalf("expected explicit up=false to be preserved")
	}
}

func TestUpsertLinkConfigInBootstrapData(t *testing.T) {
	t.Parallel()

	bootstrapData := []byte(`version: v1alpha1
machine:
  kubelet:
    nodeIP:
      validSubnets:
        - 10.71.22.160/27
---
apiVersion: v1alpha1
kind: LinkConfig
name: eth1
routes:
  - destination: 100.110.0.1/32
    gateway: 10.71.22.161
addresses:
  - address: 10.71.22.172/27
`)

	updated, changed, err := upsertLinkConfigInMachineConfig(bootstrapData, "eth1", "10.71.22.173/27")
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if !changed {
		t.Fatalf("expected bootstrap data to change")
	}

	documents, err := decodeYAMLDocuments(updated)
	if err != nil {
		t.Fatalf("failed to decode updated data: %v", err)
	}

	if len(documents) != 2 {
		t.Fatalf("expected 2 YAML documents, got %d", len(documents))
	}

	linkConfig := documents[1]
	routes, found, err := unstructured.NestedSlice(linkConfig, "routes")
	if err != nil || !found || len(routes) != 1 {
		t.Fatalf("expected routes to be preserved")
	}

	addresses, found, err := unstructured.NestedSlice(linkConfig, "addresses")
	if err != nil || !found || len(addresses) != 1 {
		t.Fatalf("expected one managed address")
	}

	entry := addresses[0].(map[string]any)
	if entry["address"] != "10.71.22.173/27" {
		t.Fatalf("unexpected managed address: %v", entry["address"])
	}
}

type fakeTalosMachineClient struct {
	activeConfig []byte
	requests     []*machineapi.ApplyConfigurationRequest
	err          error
	readErr      error
	reads        int
	closed       bool
}

func (c *fakeTalosMachineClient) ApplyConfiguration(_ context.Context, request *machineapi.ApplyConfigurationRequest, _ ...grpc.CallOption) (*machineapi.ApplyConfigurationResponse, error) {
	c.requests = append(c.requests, &machineapi.ApplyConfigurationRequest{Data: append([]byte(nil), request.Data...), Mode: request.Mode})
	if c.err == nil {
		c.activeConfig = append([]byte(nil), request.Data...)
	}
	return &machineapi.ApplyConfigurationResponse{}, c.err
}

func (c *fakeTalosMachineClient) ActiveMachineConfiguration(context.Context) ([]byte, error) {
	c.reads++
	return append([]byte(nil), c.activeConfig...), c.readErr
}

func (c *fakeTalosMachineClient) Close() error {
	c.closed = true
	return nil
}

func newInPlaceConfigFixture(t *testing.T, objects ...client.Object) (*DockyardsMachineIPReconciler, *fakeTalosMachineClient) {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, addToScheme := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		clusterv1.AddToScheme,
		bootstrapv1.AddToScheme,
		controlplanev1.AddToScheme,
		dockyardsv1.AddToScheme,
		dockyardskubevirtv1.AddToScheme,
	} {
		if err := addToScheme(scheme); err != nil {
			t.Fatalf("add test API types: %v", err)
		}
	}

	apiClient := &fakeTalosMachineClient{activeConfig: []byte("version: v1alpha1\n")}
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&dockyardskubevirtv1.DockyardsIPAMClaim{}).
		WithObjects(objects...).
		Build()

	return &DockyardsMachineIPReconciler{
		Client:    client,
		APIReader: client,
		newTalosClient: func(_ context.Context, endpoints []string, configData []byte) (talosMachineClient, error) {
			if len(endpoints) == 0 {
				t.Error("expected one or more Talos endpoints")
			}
			if !reflect.DeepEqual(endpoints, []string{"192.168.1.10", "203.0.113.10"}) && !reflect.DeepEqual(endpoints, []string{"192.168.1.10"}) {
				t.Errorf("unexpected Talos endpoints: %#v", endpoints)
			}
			if string(configData) != "test config" {
				t.Errorf("unexpected Talos config: %q", configData)
			}

			return apiClient, nil
		},
	}, apiClient
}

func TestApplyInPlaceConfigurationUsesNoRebootAndStoresHash(t *testing.T) {
	t.Parallel()

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-a", Namespace: "default"},
		Status: clusterv1.MachineStatus{
			Addresses: []clusterv1.MachineAddress{
				{Type: clusterv1.MachineInternalIP, Address: "192.168.1.10"},
				{Type: clusterv1.MachineExternalIP, Address: "203.0.113.10"},
				{Type: clusterv1.MachineInternalIP, Address: "192.168.1.10"},
				{Type: clusterv1.MachineHostName, Address: "machine-a"},
			},
		},
	}
	talosConfig := &bootstrapv1.TalosConfig{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-config", Namespace: "default"}}
	bootstrapSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-a-bootstrap-data", Namespace: "default"},
		Data:       map[string][]byte{"value": []byte("version: v1alpha1\n")},
	}
	claim := &dockyardskubevirtv1.DockyardsIPAMClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-a-external-node-ip", Namespace: "default"},
		Spec:       dockyardskubevirtv1.DockyardsIPAMClaimSpec{Address: "10.71.22.171"},
	}
	talosConfigSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a-talosconfig", Namespace: "default"},
		Data:       map[string][]byte{"talosconfig": []byte("test config")},
	}
	r, apiClient := newInPlaceConfigFixture(t, machine, talosConfig, bootstrapSecret, claim, talosConfigSecret)

	result, err := r.applyInPlaceConfiguration(context.Background(), machine, talosConfig, bootstrapSecret, claim, bootstrapSecret.Data["value"], "eth1", "10.71.22.171/27", types.NamespacedName{Namespace: "default", Name: "cluster-a"})
	if err != nil {
		t.Fatalf("applyInPlaceConfiguration: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Fatalf("unexpected requeue delay: %s", result.RequeueAfter)
	}
	if !apiClient.closed {
		t.Fatal("expected Talos client connection to close")
	}
	if len(apiClient.requests) != 1 {
		t.Fatalf("expected one Talos apply call, got %d", len(apiClient.requests))
	}
	if apiClient.requests[0].Mode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
		t.Fatalf("expected NO_REBOOT apply mode, got %s", apiClient.requests[0].Mode)
	}
	if !strings.Contains(string(apiClient.requests[0].Data), "address: 10.71.22.171/27") {
		t.Fatalf("expected updated address in applied configuration: %s", apiClient.requests[0].Data)
	}

	storedClaim := &dockyardskubevirtv1.DockyardsIPAMClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), storedClaim); err != nil {
		t.Fatalf("get IPAM claim: %v", err)
	}
	wantHash := inPlaceAppliedConfigHash("eth1", "10.71.22.171/27", "", bootstrapSecret.Data["value"])
	if storedClaim.Status.AppliedConfigHash != wantHash || storedClaim.Status.Phase != ipamClaimPhaseReady {
		t.Fatalf("unexpected applied state: %#v", storedClaim.Status)
	}
}

func TestApplyInPlaceConfigurationRepairsDuringCAPIUpdate(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name                 string
		machineAnnotations   map[string]string
		bootstrapAnnotations map[string]string
	}{
		{name: "machine update annotation", machineAnnotations: map[string]string{clusterv1.UpdateInProgressAnnotation: ""}},
		{name: "pending UpdateMachine hook", machineAnnotations: map[string]string{pendingHooksAnnotation: "AfterControlPlaneUpgrade,UpdateMachine"}},
		{name: "bootstrap update annotation", bootstrapAnnotations: map[string]string{clusterv1.UpdateInProgressAnnotation: ""}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := []byte("version: v1alpha1\n")
			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{Name: "machine-a", Namespace: "default", Annotations: tt.machineAnnotations},
				Status:     clusterv1.MachineStatus{Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "192.168.1.10"}}},
			}
			talosConfig := &bootstrapv1.TalosConfig{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-config", Namespace: "default", Annotations: tt.bootstrapAnnotations}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-bootstrap-data", Namespace: "default"}, Data: map[string][]byte{"value": data}}
			claim := &dockyardskubevirtv1.DockyardsIPAMClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "machine-a-external-node-ip", Namespace: "default"},
				Status:     dockyardskubevirtv1.DockyardsIPAMClaimStatus{AppliedConfigHash: inPlaceAppliedConfigHash("eth1", "10.71.22.171/27", "", data)},
			}
			credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a-talosconfig", Namespace: "default"}, Data: map[string][]byte{"talosconfig": []byte("test config")}}
			r, apiClient := newInPlaceConfigFixture(t, machine, talosConfig, secret, claim, credentials)
			apiClient.activeConfig = []byte(`version: v1alpha1
machine:
  install:
    image: factory.talos.dev/openstack-installer/abc:v1.13.0
  kubelet:
    nodeIP:
      validSubnets:
        - 10.71.22.160/27
---
apiVersion: v1alpha1
kind: LinkConfig
name: eth1
up: true
addresses:
  - address: 192.0.2.10/24
routes:
  - gateway: 10.71.22.161
    metric: 100
`)

			result, err := r.applyInPlaceConfiguration(t.Context(), machine, talosConfig, secret, claim, data, "eth1", "10.71.22.171/27", types.NamespacedName{Namespace: "default", Name: "cluster-a"})
			if err != nil {
				t.Fatalf("applyInPlaceConfiguration: %v", err)
			}
			if result.RequeueAfter != externalIPRepairInterval {
				t.Fatalf("expected periodic repair during upgrade, got %s", result.RequeueAfter)
			}
			if len(apiClient.requests) != 1 || apiClient.requests[0].Mode != machineapi.ApplyConfigurationRequest_NO_REBOOT {
				t.Fatalf("expected one NO_REBOOT address repair despite matching stored hash, got %v", apiClient.requests)
			}
			for _, want := range []string{"address: 10.71.22.171/27", "address: 192.0.2.10/24", "openstack-installer/abc:v1.13.0", "10.71.22.160/27", "gateway: 10.71.22.161", "metric: 100"} {
				if !strings.Contains(string(apiClient.requests[0].Data), want) {
					t.Fatalf("address repair lost expected config %q:\n%s", want, apiClient.requests[0].Data)
				}
			}

			if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			if claim.Status.Phase != ipamClaimPhaseReady {
				t.Fatalf("claim should be Ready independently of Machine update state, got %v", claim.Status)
			}

			// The next check still polls while updating, but must not reapply healthy config.
			if result, err = r.applyInPlaceConfiguration(t.Context(), machine, talosConfig, secret, claim, data, "eth1", "10.71.22.171/27", types.NamespacedName{Namespace: "default", Name: "cluster-a"}); err != nil || result.RequeueAfter != externalIPRepairInterval {
				t.Fatalf("second check: result=%v err=%v", result, err)
			}
			if len(apiClient.requests) != 1 || apiClient.reads != 2 {
				t.Fatalf("expected active config rechecked without another apply, reads=%d applies=%d", apiClient.reads, len(apiClient.requests))
			}

			// Simulate a later CABPT apply removing the address while the source hash is unchanged.
			apiClient.activeConfig = data
			if _, err = r.applyInPlaceConfiguration(t.Context(), machine, talosConfig, secret, claim, data, "eth1", "10.71.22.171/27", types.NamespacedName{Namespace: "default", Name: "cluster-a"}); err != nil {
				t.Fatalf("repair after CABPT reapply: %v", err)
			}
			if len(apiClient.requests) != 2 {
				t.Fatal("expected removed IP to be repaired again despite identical stored hash")
			}
		})
	}
}

func TestMachineIPReconcileRepairsBootstrapAndActiveConfigDuringUpdate(t *testing.T) {
	t.Parallel()

	for _, failure := range []string{"none", "read", "apply", "connect"} {
		t.Run(failure, func(t *testing.T) {
			machine := &clusterv1.Machine{
				ObjectMeta: metav1.ObjectMeta{
					Name: "machine-a", Namespace: "default", UID: "machine-uid",
					Labels:      map[string]string{clusterv1.MachineControlPlaneLabel: ""},
					Annotations: map[string]string{clusterv1.UpdateInProgressAnnotation: "", pendingHooksAnnotation: updateMachineHookName},
				},
				Spec: clusterv1.MachineSpec{
					ClusterName: "cluster-a",
					Bootstrap: clusterv1.Bootstrap{ConfigRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: bootstrapv1.GroupVersion.Group, Kind: "TalosConfig", Name: "machine-a-config",
					}},
				},
				Status: clusterv1.MachineStatus{
					NodeRef:   clusterv1.MachineNodeReference{Name: "machine-a"},
					Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "192.168.1.10"}},
				},
			}
			cluster := &dockyardsv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
				Spec: dockyardsv1.ClusterSpec{Advanced: dockyardsv1.ClusterAdvancedOptions{Kubevirt: dockyardsv1.ClusterKubevirtOptions{Talos: dockyardsv1.ClusterTalosOptions{
					ExternalNodeInterface: "eth1", ExternalNodeIPv4Subnet: "10.71.22.160/27",
				}}}},
			}
			capiCluster := &clusterv1.Cluster{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster-a", Namespace: "default"},
				Spec: clusterv1.ClusterSpec{ControlPlaneRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: controlplanev1.GroupVersion.Group, Kind: "TalosControlPlane", Name: "controlplane-a",
				}},
			}
			controlPlane := &controlplanev1.TalosControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "controlplane-a", Namespace: "default"},
				Spec:       controlplanev1.TalosControlPlaneSpec{Replicas: ptr.To[int32](3)},
			}
			talosConfig := &bootstrapv1.TalosConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "machine-a-config", Namespace: "default", Annotations: map[string]string{clusterv1.UpdateInProgressAnnotation: ""}},
				Status:     bootstrapv1.TalosConfigStatus{DataSecretName: ptr.To("machine-a-bootstrap-data")},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "machine-a-bootstrap-data", Namespace: "default", Annotations: map[string]string{talosInPlaceConfigHashAnnotation: "cabpt-hash"}},
				Data:       map[string][]byte{"value": []byte("version: v1alpha1\nmachine:\n  install:\n    image: ghcr.io/siderolabs/installer:v1.13.0\n")},
			}
			claim := &dockyardskubevirtv1.DockyardsIPAMClaim{
				ObjectMeta: metav1.ObjectMeta{Name: machineIPAMClaimName(machine.Name), Namespace: machine.Namespace, OwnerReferences: []metav1.OwnerReference{machineOwnerReference(machine)}},
				Spec: dockyardskubevirtv1.DockyardsIPAMClaimSpec{
					ClusterName: "cluster-a", MachineName: machine.Name, Address: "10.71.22.164", Subnet: "10.71.22.160/27", Interface: "eth1", ControlPlane: true,
				},
				Status: dockyardskubevirtv1.DockyardsIPAMClaimStatus{Phase: ipamClaimPhasePending, Reason: "WaitingForCAPIUpdate", AppliedConfigHash: "previous-apply"},
			}
			credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a-talosconfig", Namespace: "default"}, Data: map[string][]byte{"talosconfig": []byte("test config")}}
			r, apiClient := newInPlaceConfigFixture(t, machine, cluster, capiCluster, controlPlane, talosConfig, secret, claim, credentials)
			switch failure {
			case "read":
				apiClient.readErr = errors.New("node rebooting")
			case "apply":
				apiClient.err = errors.New("node rebooting")
			case "connect":
				r.newTalosClient = func(ctx context.Context, _ []string, _ []byte) (talosMachineClient, error) {
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > externalIPTalosTimeout {
						t.Fatal("Talos API calls must have a bounded timeout")
					}
					return nil, errors.New("node unreachable")
				}
			}

			result, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(machine)})
			if err != nil || result.RequeueAfter != externalIPRepairInterval {
				t.Fatalf("expected bounded retry during update, got result=%v err=%v", result, err)
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(secret), secret); err != nil {
				t.Fatal(err)
			}
			if secret.Annotations[talosInPlaceConfigHashAnnotation] != "cabpt-hash" || !strings.Contains(string(secret.Data["value"]), "address: 10.71.22.164/27") || !strings.Contains(string(secret.Data["value"]), "installer:v1.13.0") {
				t.Fatal("bootstrap repair must add claimed IP and preserve CABPT hash and installer image")
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(claim), claim); err != nil {
				t.Fatal(err)
			}
			if failure == "none" {
				if claim.Status.Phase != ipamClaimPhaseReady || !strings.Contains(string(apiClient.activeConfig), "address: 10.71.22.164/27") {
					t.Fatalf("expected Ready claim and repaired active config, got %v", claim.Status)
				}
			} else if claim.Status.Phase != ipamClaimPhasePending || claim.Status.Reason != "RetryingIPConfiguration" || claim.Status.AppliedConfigHash != "previous-apply" {
				t.Fatalf("transient Talos failures must remain retryable without recording successful apply: %v", claim.Status)
			}
			if err := r.Get(t.Context(), client.ObjectKeyFromObject(machine), machine); err != nil {
				t.Fatal(err)
			}
			if !hasCAPIUpdateInProgress(machine) || machine.Annotations[pendingHooksAnnotation] != updateMachineHookName {
				t.Fatal("IP repair must leave CAPI update coordination intact")
			}
		})
	}
}

func TestHasCAPIUpdateInProgressIgnoresOtherPendingHooks(t *testing.T) {
	t.Parallel()

	machine := &clusterv1.Machine{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		pendingHooksAnnotation: "AfterControlPlaneUpgrade,AfterWorkersUpgrade",
	}}}
	if hasCAPIUpdateInProgress(machine) {
		t.Fatal("unrelated pending hooks should not block IP configuration")
	}
}

func TestApplyInPlaceConfigurationIsIdempotent(t *testing.T) {
	t.Parallel()

	data := []byte("version: v1alpha1\n")
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-a", Namespace: "default"},
		Status:     clusterv1.MachineStatus{Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "192.168.1.10"}}},
	}
	talosConfig := &bootstrapv1.TalosConfig{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-config", Namespace: "default"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-bootstrap-data", Namespace: "default"}, Data: map[string][]byte{"value": data}}
	claim := &dockyardskubevirtv1.DockyardsIPAMClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-a-external-node-ip", Namespace: "default"},
		Status: dockyardskubevirtv1.DockyardsIPAMClaimStatus{
			AppliedConfigHash: inPlaceAppliedConfigHash("eth1", "10.71.22.171/27", "", data),
		},
	}
	talosConfigSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a-talosconfig", Namespace: "default"}, Data: map[string][]byte{"talosconfig": []byte("test config")}}
	r, apiClient := newInPlaceConfigFixture(t, machine, talosConfig, secret, claim, talosConfigSecret)
	apiClient.activeConfig = []byte("version: v1alpha1\n---\napiVersion: v1alpha1\nkind: LinkConfig\nname: eth1\nup: true\naddresses:\n  - address: 10.71.22.171/27\n")

	if _, err := r.applyInPlaceConfiguration(context.Background(), machine, talosConfig, secret, claim, data, "eth1", "10.71.22.171/27", types.NamespacedName{Namespace: "default", Name: "cluster-a"}); err != nil {
		t.Fatalf("applyInPlaceConfiguration: %v", err)
	}
	if len(apiClient.requests) != 0 {
		t.Fatal("expected healthy active config to be left unchanged")
	}
	if apiClient.reads != 1 {
		t.Fatal("stored hash must not suppress active configuration verification")
	}
}

func TestUpsertLinkConfigInMachineConfigPreservesRoutesAndDocuments(t *testing.T) {
	t.Parallel()

	data := []byte(`version: v1alpha1
machine: {}
---
apiVersion: v1alpha1
kind: LinkConfig
name: eth0
routes:
  - destination: 83.255.255.26/32
    gateway: 100.66.2.1
---
apiVersion: v1alpha1
kind: LinkConfig
name: eth1
routes:
  - destination: 10.71.22.90/32
    gateway: 100.66.2.1
  - gateway: 10.71.22.161
addresses:
  - address: 10.71.22.172/27
---
apiVersion: v1alpha1
kind: AuthenticationConfiguration
anonymous: null
`)

	updated, changed, err := upsertLinkConfigInMachineConfig(data, "eth1", "10.71.22.173/27")
	if err != nil {
		t.Fatalf("upsertLinkConfigInMachineConfig: %v", err)
	}
	if !changed {
		t.Fatal("expected configuration to change")
	}

	if !strings.Contains(string(updated), "83.255.255.26/32") ||
		!strings.Contains(string(updated), "10.71.22.90/32") ||
		!strings.Contains(string(updated), "gateway: 10.71.22.161") ||
		!strings.Contains(string(updated), "kind: AuthenticationConfiguration") {
		t.Fatalf("updated configuration lost existing content:\n%s", updated)
	}
	if strings.Contains(string(updated), "address: 10.71.22.172/27") {
		t.Fatalf("old address was not replaced:\n%s", updated)
	}
}

func TestApplyInPlaceConfigurationApplyFailureDoesNotStoreHash(t *testing.T) {
	t.Parallel()

	applyErr := errors.New("Talos API unavailable")
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: "machine-a", Namespace: "default"},
		Status:     clusterv1.MachineStatus{Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "192.168.1.10"}}},
	}
	talosConfig := &bootstrapv1.TalosConfig{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-config", Namespace: "default"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-bootstrap-data", Namespace: "default"}, Data: map[string][]byte{"value": []byte("config")}}
	claim := &dockyardskubevirtv1.DockyardsIPAMClaim{ObjectMeta: metav1.ObjectMeta{Name: "machine-a-external-node-ip", Namespace: "default"}}
	talosConfigSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "cluster-a-talosconfig", Namespace: "default"}, Data: map[string][]byte{"talosconfig": []byte("test config")}}
	r, apiClient := newInPlaceConfigFixture(t, machine, talosConfig, secret, claim, talosConfigSecret)
	apiClient.err = applyErr

	_, err := r.applyInPlaceConfiguration(context.Background(), machine, talosConfig, secret, claim, secret.Data["value"], "eth1", "10.71.22.171/27", types.NamespacedName{Namespace: "default", Name: "cluster-a"})
	if !errors.Is(err, applyErr) {
		t.Fatalf("expected Talos API error, got %v", err)
	}
	storedClaim := &dockyardskubevirtv1.DockyardsIPAMClaim{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(claim), storedClaim); err != nil {
		t.Fatalf("get IPAM claim: %v", err)
	}
	if storedClaim.Status.AppliedConfigHash != "" {
		t.Fatalf("failed apply must not be recorded as successful: %q", storedClaim.Status.AppliedConfigHash)
	}
}
