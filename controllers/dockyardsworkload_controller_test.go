// Copyright 2025 Sudo Sweden AB
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
	"testing"

	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestClusterGatewayParentReference(t *testing.T) {
	t.Run("returns parent ref when name and namespace are set", func(t *testing.T) {
		cluster := map[string]any{
			"spec": map[string]any{
				"advanced": map[string]any{
					"gateway": map[string]any{
						"parentRef": map[string]any{
							"name":      "shared-gateway",
							"namespace": "gateway-system",
						},
					},
				},
			},
		}

		parentRef, found, err := clusterGatewayParentReference(cluster)
		if err != nil {
			t.Fatal(err)
		}

		if !found {
			t.Fatal("expected to find gateway parent ref override")
		}

		if parentRef.Name != gatewayapiv1.ObjectName("shared-gateway") {
			t.Fatalf("expected parent ref name %q, got %q", "shared-gateway", parentRef.Name)
		}

		if parentRef.Namespace == nil {
			t.Fatal("expected parent ref namespace to be set")
		}

		if *parentRef.Namespace != gatewayapiv1.Namespace("gateway-system") {
			t.Fatalf("expected parent ref namespace %q, got %q", "gateway-system", *parentRef.Namespace)
		}
	})

	t.Run("returns not found when name is missing", func(t *testing.T) {
		cluster := map[string]any{
			"spec": map[string]any{
				"advanced": map[string]any{
					"gateway": map[string]any{
						"parentRef": map[string]any{
							"namespace": "gateway-system",
						},
					},
				},
			},
		}

		_, found, err := clusterGatewayParentReference(cluster)
		if err != nil {
			t.Fatal(err)
		}

		if found {
			t.Fatal("expected no gateway parent ref override")
		}
	})

	t.Run("returns not found when namespace is missing", func(t *testing.T) {
		cluster := map[string]any{
			"spec": map[string]any{
				"advanced": map[string]any{
					"gateway": map[string]any{
						"parentRef": map[string]any{
							"name": "shared-gateway",
						},
					},
				},
			},
		}

		_, found, err := clusterGatewayParentReference(cluster)
		if err != nil {
			t.Fatal(err)
		}

		if found {
			t.Fatal("expected no gateway parent ref override")
		}
	})

	t.Run("returns not found for empty values", func(t *testing.T) {
		cluster := map[string]any{
			"spec": map[string]any{
				"advanced": map[string]any{
					"gateway": map[string]any{
						"parentRef": map[string]any{
							"name":      "   ",
							"namespace": "  ",
						},
					},
				},
			},
		}

		_, found, err := clusterGatewayParentReference(cluster)
		if err != nil {
			t.Fatal(err)
		}

		if found {
			t.Fatal("expected no gateway parent ref override")
		}
	})
}
