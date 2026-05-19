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
	dyconfig "github.com/sudoswedenab/dockyards-backend/api/config"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"github.com/sudoswedenab/dockyards-kubevirt/test/mockcrds"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

func TestDockyardsClusterReconciler_ReconcileAPIEndpoint(t *testing.T) {
	t.Run("test valid listener", func(t *testing.T) {
		config := dyconfig.NewFakeConfigManager(map[string]string{
			string(dyconfig.KeyExternalURL): "http://testing.dockyards.dev",
		})

		r := DockyardsClusterReconciler{DockyardsConfig: config}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "testing",
				Name:      "test",
			},
		}
		_, err := r.reconcileAPIEndpoint(context.TODO(), &cluster)
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
		config := dyconfig.NewFakeConfigManager(map[string]string{
			string(dyconfig.KeyExternalURL): "",
		})

		r := DockyardsClusterReconciler{DockyardsConfig: config}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "testing",
				Name:      "test",
			},
		}

		_, err := r.reconcileAPIEndpoint(context.TODO(), &cluster)
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

func TestResolveClusterGatewayParentReference(t *testing.T) {
	t.Run("falls back to default parent ref", func(t *testing.T) {
		scheme := runtime.NewScheme()
		_ = dockyardsv1.AddToScheme(scheme)

		cluster := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": dockyardsv1.GroupVersion.String(),
			"kind":       dockyardsv1.ClusterKind,
			"metadata": map[string]any{
				"name":      "cluster-b",
				"namespace": "tenant-b",
			},
		}}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cluster).Build()

		fallback := gatewayapiv1.ParentReference{
			Name:      gatewayapiv1.ObjectName("default-gw"),
			Namespace: ptr.To(gatewayapiv1.Namespace("default-system")),
		}

		resolved, err := resolveClusterGatewayParentReference(context.Background(), c, &dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster-b", Namespace: "tenant-b"},
		}, fallback)
		if err != nil {
			t.Fatal(err)
		}

		if !cmp.Equal(resolved, fallback) {
			t.Fatalf("expected fallback parent ref, diff: %s", cmp.Diff(fallback, resolved))
		}
	})
}

func TestDockyardsClusterReconciler_ReconcileTLSRouteUsesResolvedParentRef(t *testing.T) {
	scheme := runtime.NewScheme()

	_ = dockyardsv1.AddToScheme(scheme)
	_ = gatewayapiv1.Install(scheme)
	_ = gatewayapiv1alpha2.Install(scheme)

	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := DockyardsClusterReconciler{
		Client: c,
	}

	cluster := dockyardsv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-c",
			Namespace: "tenant-c",
			UID:       "cluster-c-uid",
		},
		Status: dockyardsv1.ClusterStatus{
			APIEndpoint: dockyardsv1.ClusterAPIEndpoint{
				Host: "tenant-c-cluster-c.example.com",
				Port: 6443,
			},
		},
	}

	parentRef := gatewayapiv1.ParentReference{
		Name:      gatewayapiv1.ObjectName("shared-gw"),
		Namespace: ptr.To(gatewayapiv1.Namespace("gateway-system")),
	}

	_, err := r.reconcileTLSRoute(context.Background(), &cluster, parentRef)
	if err != nil {
		t.Fatal(err)
	}

	actual := gatewayapiv1alpha2.TLSRoute{}
	err = c.Get(context.Background(), client.ObjectKey{Name: "cluster-c", Namespace: "tenant-c"}, &actual)
	if err != nil {
		t.Fatal(err)
	}

	if len(actual.Spec.ParentRefs) != 1 {
		t.Fatalf("expected one parent ref, got %d", len(actual.Spec.ParentRefs))
	}

	if !cmp.Equal(actual.Spec.ParentRefs[0], parentRef) {
		t.Fatalf("unexpected parent ref diff: %s", cmp.Diff(parentRef, actual.Spec.ParentRefs[0]))
	}
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

	dockyardsConfig := dyconfig.NewFakeConfigManager(map[string]string{})

	t.Run("test workload", func(t *testing.T) {
		r := DockyardsClusterReconciler{
			Client:          mgr.GetClient(),
			DockyardsConfig: dockyardsConfig,
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
					Namespace: ptr.To("dockyards-public"),
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
			EnableWorkloadIngress: true,
			DockyardsConfig:       dockyardsConfig,
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
					Namespace: ptr.To("dockyards-public"),
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

	t.Run("test no default ingress provider skips workload", func(t *testing.T) {
		r := DockyardsClusterReconciler{
			Client:                mgr.GetClient(),
			EnableWorkloadIngress: true,
			DockyardsConfig:       dockyardsConfig,
		}

		cluster := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
			Spec: dockyardsv1.ClusterSpec{
				NoDefaultIngressProvider: true,
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
			Spec:       cluster.Spec,
			Status: dockyardsv1.ClusterStatus{
				Conditions: []metav1.Condition{
					{
						Type:   IngressWorkloadReconciledCondition,
						Reason: NoDefaultIngressProviderReason,
						Status: metav1.ConditionTrue,
					},
				},
			},
		}

		ignoreConditionFields := cmpopts.IgnoreFields(metav1.Condition{}, "ObservedGeneration", "LastTransitionTime")
		if !cmp.Equal(cluster, expected, ignoreConditionFields) {
			t.Errorf("diff: %s", cmp.Diff(expected, cluster, ignoreConditionFields))
		}

		workloadKey := client.ObjectKey{
			Name:      cluster.Name + "-ingress-nginx",
			Namespace: namespace.Name,
		}

		var actual dockyardsv1.Workload
		err = c.Get(ctx, workloadKey, &actual)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("expected workload to not exist, got error: %v", err)
		}
	})

	t.Run("test workload gateway missing address", func(t *testing.T) {
		r := DockyardsClusterReconciler{
			Client:                   mgr.GetClient(),
			DockyardsSystemNamespace: "dockyards-testing",
			EnableWorkloadIngress:    true,
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
