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
	"errors"
	"net/netip"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

func TestNewIPv4Range(t *testing.T) {
	t.Parallel()

	prefix := netip.MustParsePrefix("10.71.22.160/27")
	rangeConfig, err := newIPv4Range(prefix)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	if got, want := uint32ToIPv4(rangeConfig.controlPlaneStart).String(), "10.71.22.162"; got != want {
		t.Fatalf("unexpected controlPlaneStart: got %q, want %q", got, want)
	}

	if got, want := uint32ToIPv4(rangeConfig.controlPlaneEnd).String(), "10.71.22.170"; got != want {
		t.Fatalf("unexpected controlPlaneEnd: got %q, want %q", got, want)
	}

	if got, want := uint32ToIPv4(rangeConfig.workerStart).String(), "10.71.22.171"; got != want {
		t.Fatalf("unexpected workerStart: got %q, want %q", got, want)
	}

	if got, want := uint32ToIPv4(rangeConfig.workerEnd).String(), "10.71.22.190"; got != want {
		t.Fatalf("unexpected workerEnd: got %q, want %q", got, want)
	}
}

func TestAllocateReuseAndRoleSeparation(t *testing.T) {
	t.Parallel()

	rangeConfig, err := newIPv4Range(netip.MustParsePrefix("10.71.22.160/27"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	state := newIPAMState()

	cp1, err := state.allocate(true, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got, want := cp1.String(), "10.71.22.162"; got != want {
		t.Fatalf("unexpected first cp ip: got %q, want %q", got, want)
	}

	state.Leases["cp-1"] = ipLease{IP: cp1, ControlPlane: true}

	worker1, err := state.allocate(false, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got, want := worker1.String(), "10.71.22.171"; got != want {
		t.Fatalf("unexpected first worker ip: got %q, want %q", got, want)
	}

	state.Leases["worker-1"] = ipLease{IP: worker1, ControlPlane: false}

	delete(state.Leases, "worker-1")
	state.release(ipLease{IP: worker1, ControlPlane: false}, rangeConfig)

	worker2, err := state.allocate(false, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got, want := worker2.String(), worker1.String(); got != want {
		t.Fatalf("expected released worker ip to be reused, got %q want %q", got, want)
	}

	state.Leases["worker-2"] = ipLease{IP: worker2, ControlPlane: false}

	delete(state.Leases, "cp-1")
	state.release(ipLease{IP: cp1, ControlPlane: true}, rangeConfig)

	cp2, err := state.allocate(true, rangeConfig)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if got, want := cp2.String(), cp1.String(); got != want {
		t.Fatalf("expected released cp ip to be reused, got %q want %q", got, want)
	}
}

func TestControlPlaneReserveExhaustion(t *testing.T) {
	t.Parallel()

	rangeConfig, err := newIPv4Range(netip.MustParsePrefix("10.71.22.160/27"))
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}

	state := newIPAMState()

	for i := 0; i < reservedControlPlaneIPs; i++ {
		ip, allocErr := state.allocate(true, rangeConfig)
		if allocErr != nil {
			t.Fatalf("expected allocation to succeed on slot %d: %v", i, allocErr)
		}

		state.Leases["cp-"+ip.String()] = ipLease{IP: ip, ControlPlane: true}
	}

	_, err = state.allocate(true, rangeConfig)
	if !errors.Is(err, errNoControlPlaneIPsAvailable) {
		t.Fatalf("expected errNoControlPlaneIPsAvailable, got %v", err)
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

	result, changed, err = upsertLinkConfigPatch(result, "eth1", "10.71.22.171/27")
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
