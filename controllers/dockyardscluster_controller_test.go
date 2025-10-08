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
	"log/slog"
	"os"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"github.com/sudoswedenab/dockyards-kubevirt/test/mockcrds"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
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

func TestDockyardsClusterReconciler_ReconcileIngressNginx(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("no kubebuilder assets configured")
	}

	env := envtest.Environment{
		CRDs: mockcrds.CRDs,
	}

	textHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})
	slogr := logr.FromSlogHandler(textHandler)

	ctrl.SetLogger(slogr)

	ctx, cancel := context.WithCancel(context.Background())

	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		cancel()
		err := env.Stop()
		if err != nil {
			panic(err)
		}
	})

	scheme := runtime.NewScheme()

	_ = corev1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	namespace := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "test-",
		},
	}

	err = c.Create(ctx, &namespace)
	if err != nil {
		t.Fatal(err)
	}

	mgr, err := manager.New(cfg, manager.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	go func() {
		err := mgr.Start(ctx)
		if err != nil {
			panic(err)
		}
	}()

	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatalf("could not sync cache")
	}

	ignoreFields := cmpopts.IgnoreFields(metav1.ObjectMeta{}, "UID", "CreationTimestamp", "ManagedFields", "ResourceVersion", "Generation")

	t.Run("test workload", func(t *testing.T) {
		r := DockyardsClusterReconciler{
			Client:             mgr.GetClient(),
			DockyardsNamespace: "dockyards-testing",
		}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
		}

		err := c.Create(ctx, &cluster)
		if err != nil {
			t.Fatal(err)
		}

		gateway := gatewayapiv1.Gateway{}

		_, err = r.reconcileIngressNginx(ctx, &cluster, &gateway)
		if err != nil {
			t.Fatal(err)
		}

		expected := dockyardsv1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					dockyardsv1.LabelClusterName: cluster.Name,
				},
				Name:      cluster.Name + "-ingress-nginx",
				Namespace: namespace.Name,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: dockyardsv1.GroupVersion.String(),
						Kind:       dockyardsv1.ClusterKind,
						Name:       cluster.Name,
						UID:        cluster.UID,
					},
				},
			},
			Spec: dockyardsv1.WorkloadSpec{
				ClusterComponent: true,
				TargetNamespace:  "ingress-nginx",
				Provenience:      dockyardsv1.ProvenienceDockyards,
				WorkloadTemplateRef: &corev1.TypedObjectReference{
					Kind:      dockyardsv1.WorkloadTemplateKind,
					Name:      "ingress-nginx",
					Namespace: &r.DockyardsNamespace,
				},
			},
		}

		var actual dockyardsv1.Workload
		err = c.Get(ctx, client.ObjectKeyFromObject(&expected), &actual)
		if err != nil {
			t.Fatal(err)
		}

		if !cmp.Equal(actual, expected, ignoreFields) {
			t.Errorf("diff: %s", cmp.Diff(expected, actual, ignoreFields))
		}
	})

	t.Run("test workload ingress", func(t *testing.T) {
		r := DockyardsClusterReconciler{
			Client:                mgr.GetClient(),
			DockyardsNamespace:    "dockyards-testing",
			EnableWorkloadIngress: true,
		}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
		}

		err := c.Create(ctx, &cluster)
		if err != nil {
			t.Fatal(err)
		}

		gateway := gatewayapiv1.Gateway{
			Status: gatewayapiv1.GatewayStatus{
				Addresses: []gatewayapiv1.GatewayStatusAddress{
					{
						Type:  ptr.To(gatewayapiv1.IPAddressType),
						Value: "1.2.3.4",
					},
				},
			},
		}

		_, err = r.reconcileIngressNginx(ctx, &cluster, &gateway)
		if err != nil {
			t.Fatal(err)
		}

		expected := dockyardsv1.Workload{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					dockyardsv1.LabelClusterName: cluster.Name,
				},
				Name:      cluster.Name + "-ingress-nginx",
				Namespace: namespace.Name,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: dockyardsv1.GroupVersion.String(),
						Kind:       dockyardsv1.ClusterKind,
						Name:       cluster.Name,
						UID:        cluster.UID,
					},
				},
			},
			Spec: dockyardsv1.WorkloadSpec{
				ClusterComponent: true,
				TargetNamespace:  "ingress-nginx",
				Provenience:      dockyardsv1.ProvenienceDockyards,
				WorkloadTemplateRef: &corev1.TypedObjectReference{
					Kind:      dockyardsv1.WorkloadTemplateKind,
					Name:      "ingress-nginx",
					Namespace: &r.DockyardsNamespace,
				},
				Input: &apiextensionsv1.JSON{
					Raw: []byte(`{"service":{"loadBalancerIP":"1.2.3.4"}}`),
				},
			},
		}

		var actual dockyardsv1.Workload
		err = c.Get(ctx, client.ObjectKeyFromObject(&expected), &actual)
		if err != nil {
			t.Fatal(err)
		}

		if !cmp.Equal(actual, expected, ignoreFields) {
			t.Errorf("diff: %s", cmp.Diff(expected, actual, ignoreFields))
		}
	})

	t.Run("test workload gateway missing address", func(t *testing.T) {
		r := DockyardsClusterReconciler{
			Client:                mgr.GetClient(),
			DockyardsNamespace:    "dockyards-testing",
			EnableWorkloadIngress: true,
		}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
		}

		err := c.Create(ctx, &cluster)
		if err != nil {
			t.Fatal(err)
		}

		gateway := gatewayapiv1.Gateway{
			Status: gatewayapiv1.GatewayStatus{
				Addresses: []gatewayapiv1.GatewayStatusAddress{},
			},
		}

		_, err = r.reconcileIngressNginx(ctx, &cluster, &gateway)
		if err != nil {
			t.Fatal(err)
		}

		expected := dockyardsv1.Cluster{
			ObjectMeta: cluster.ObjectMeta,
			Status: dockyardsv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{
						Type:   IngressWorkloadReconciledCondition,
						Reason: WaitingForGatewayAddressReason,
						Status: metav1.ConditionFalse,
					},
				},
			},
		}

		ignoreFields := cmpopts.IgnoreFields(metav1.Condition{}, "ObservedGeneration", "LastTransitionTime")

		if !cmp.Equal(cluster, expected, ignoreFields) {
			t.Errorf("diff: %s", cmp.Diff(expected, cluster, ignoreFields))
		}
	})
}
