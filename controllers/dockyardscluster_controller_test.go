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
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestDockyardsClusterReconciler_ReconcileAPIEndpoint(t *testing.T) {
	r := DockyardsClusterReconciler{}

	t.Run("test valid listener", func(t *testing.T) {
		gateway := gatewayapiv1.Gateway{
			Spec: gatewayapiv1.GatewaySpec{
				Listeners: []gatewayapiv1.Listener{
					{
						Hostname: ptr.To(gatewayapiv1.Hostname("testing.dockyards.dev")),
						AllowedRoutes: &gatewayapiv1.AllowedRoutes{
							Namespaces: &gatewayapiv1.RouteNamespaces{
								Selector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"app.kubernetes.io/part-of": "dockyards",
									},
								},
							},
						},
					},
				},
			},
		}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "testing",
				Name:      "test",
			},
		}

		_, err := r.reconcileAPIEndpoint(context.TODO(), &cluster, &gateway)
		if err != nil {
			t.Fatal(err)
		}

		expected := dockyardsv1.Cluster{
			ObjectMeta: cluster.ObjectMeta,
			Status: dockyardsv1.ClusterStatus{
				APIEndpoint: dockyardsv1.ClusterAPIEndpoint{
					Host: "testing-test.testing.dockyards.dev",
					Port: 6443,
				},
				Conditions: []metav1.Condition{
					{
						Reason: ReconciledReason,
						Status: metav1.ConditionTrue,
						Type:   APIEndpointReconciledCondition,
					},
				},
			},
		}

		opts := cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime")

		if !cmp.Equal(cluster, expected, opts) {
			t.Errorf("diff: %s", cmp.Diff(expected, cluster, opts))
		}
	})

	t.Run("test missing hostname", func(t *testing.T) {
		gateway := gatewayapiv1.Gateway{
			Spec: gatewayapiv1.GatewaySpec{
				Listeners: []gatewayapiv1.Listener{
					{
						AllowedRoutes: &gatewayapiv1.AllowedRoutes{
							Namespaces: &gatewayapiv1.RouteNamespaces{
								Selector: &metav1.LabelSelector{
									MatchLabels: map[string]string{
										"app.kubernetes.io/part-of": "dockyards",
									},
								},
							},
						},
					},
				},
			},
		}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "testing",
				Name:      "test",
			},
		}

		_, err := r.reconcileAPIEndpoint(context.TODO(), &cluster, &gateway)
		if err != nil {
			t.Fatal(err)
		}

		expected := dockyardsv1.Cluster{
			ObjectMeta: cluster.ObjectMeta,
			Status: dockyardsv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{
						Reason: WaitingForListenerHostnameReason,
						Status: metav1.ConditionFalse,
						Type:   APIEndpointReconciledCondition,
					},
				},
			},
		}

		opts := cmpopts.IgnoreFields(metav1.Condition{}, "LastTransitionTime")

		if !cmp.Equal(cluster, expected, opts) {
			t.Errorf("diff: %s", cmp.Diff(expected, cluster, opts))
		}
	})
}
