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
	"testing"

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestClusterAPIClusterReconciler_reconcileClusterClassTopology(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clusterv1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)

	namespace := "test"
	clusterName := "example"

	dockyardsCluster := dockyardsv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
		Spec: dockyardsv1.ClusterSpec{
			Version: "v1.35.3",
		},
	}

	controlPlaneReplicas := int32(3)
	workerReplicas := int32(5)

	controlPlaneNodePool := dockyardsv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cp-0",
			Namespace: namespace,
			Labels: map[string]string{
				dockyardsv1.LabelClusterName: clusterName,
			},
		},
		Spec: dockyardsv1.NodePoolSpec{
			ControlPlane: true,
			Replicas:     &controlPlaneReplicas,
		},
	}

	workerNodePool := dockyardsv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: namespace,
			Labels: map[string]string{
				dockyardsv1.LabelClusterName: clusterName,
			},
		},
		Spec: dockyardsv1.NodePoolSpec{
			Replicas: &workerReplicas,
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&dockyardsCluster, &controlPlaneNodePool, &workerNodePool).
		Build()

	r := &ClusterAPIClusterReconciler{Client: c}

	cluster := clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace}}

	_, err := r.reconcileClusterClassTopology(context.Background(), &cluster)
	if err != nil {
		t.Fatal(err)
	}

	if cluster.Spec.Topology == nil {
		t.Fatal("expected cluster topology to be set")
	}

	if cluster.Spec.Topology.Class != "example-class" {
		t.Fatalf("expected topology class %q, got %q", "example-class", cluster.Spec.Topology.Class)
	}

	if cluster.Spec.Topology.Version != "v1.35.3" {
		t.Fatalf("expected topology version %q, got %q", "v1.35.3", cluster.Spec.Topology.Version)
	}

	if cluster.Spec.Topology.ControlPlane.Replicas == nil || *cluster.Spec.Topology.ControlPlane.Replicas != controlPlaneReplicas {
		t.Fatalf("expected control plane replicas %d", controlPlaneReplicas)
	}

	if cluster.Spec.ControlPlaneRef != nil {
		t.Fatalf("expected control plane ref to be nil in topology mode")
	}

	if cluster.Spec.InfrastructureRef != nil {
		t.Fatalf("expected infrastructure ref to be nil in topology mode")
	}

	if cluster.Spec.Topology.Workers == nil || len(cluster.Spec.Topology.Workers.MachineDeployments) != 1 {
		t.Fatalf("expected one worker topology machine deployment")
	}

	workerTopology := cluster.Spec.Topology.Workers.MachineDeployments[0]
	if workerTopology.Class != workerNodePool.Name || workerTopology.Name != workerNodePool.Name {
		t.Fatalf("unexpected worker topology class/name: %#v", workerTopology)
	}

	if workerTopology.Replicas == nil || *workerTopology.Replicas != workerReplicas {
		t.Fatalf("expected worker replicas %d", workerReplicas)
	}

	var clusterClass clusterv1.ClusterClass
	err = c.Get(context.Background(), client.ObjectKey{Name: "example-class", Namespace: namespace}, &clusterClass)
	if err != nil {
		t.Fatal(err)
	}

	if clusterClass.Spec.ControlPlane.Ref == nil || clusterClass.Spec.ControlPlane.Ref.Kind != "TalosControlPlane" {
		t.Fatalf("expected control plane template ref to TalosControlPlane")
	}

	if clusterClass.Spec.ControlPlane.Ref.Name != controlPlaneNodePool.Name {
		t.Fatalf("expected control plane template ref name %q, got %q", controlPlaneNodePool.Name, clusterClass.Spec.ControlPlane.Ref.Name)
	}

	if len(clusterClass.Spec.Workers.MachineDeployments) != 1 {
		t.Fatalf("expected one worker machine deployment class")
	}

	if clusterClass.Spec.Workers.MachineDeployments[0].Class != workerNodePool.Name {
		t.Fatalf("expected worker class %q, got %q", workerNodePool.Name, clusterClass.Spec.Workers.MachineDeployments[0].Class)
	}

	var kubevirtClusterTemplate providerv1.KubevirtClusterTemplate
	err = c.Get(context.Background(), client.ObjectKey{Name: "example-cluster-template", Namespace: namespace}, &kubevirtClusterTemplate)
	if err != nil {
		t.Fatal(err)
	}

	serviceType := kubevirtClusterTemplate.Spec.Template.Spec.ControlPlaneServiceTemplate.Spec.Type
	if serviceType != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("expected service type %q, got %q", corev1.ServiceTypeLoadBalancer, serviceType)
	}
}

func TestClusterAPIClusterReconciler_reconcileClusterClassTopologyWithoutControlPlaneNodePool(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clusterv1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)

	namespace := "test"
	clusterName := "example"

	dockyardsCluster := dockyardsv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace},
		Spec:       dockyardsv1.ClusterSpec{Version: "v1.35.3"},
	}

	workerNodePool := dockyardsv1.NodePool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-0",
			Namespace: namespace,
			Labels: map[string]string{
				dockyardsv1.LabelClusterName: clusterName,
			},
		},
		Spec: dockyardsv1.NodePoolSpec{Replicas: ptr.To(int32(1))},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&dockyardsCluster, &workerNodePool).Build()
	r := &ClusterAPIClusterReconciler{Client: c}
	cluster := clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: clusterName, Namespace: namespace}}

	_, err := r.reconcileClusterClassTopology(context.Background(), &cluster)
	if err != nil {
		t.Fatal(err)
	}

	if cluster.Spec.Topology != nil {
		t.Fatalf("expected topology to remain nil when no control plane node pool exists")
	}

	var clusterClass clusterv1.ClusterClass
	err = c.Get(context.Background(), client.ObjectKey{Name: "example-class", Namespace: namespace}, &clusterClass)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected cluster class to not exist, got err: %v", err)
	}
}

func TestClusterAPIClusterReconciler_ReconcileLegacyModeSetsInfrastructureRef(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clusterv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)

	cluster := clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "legacy",
			Namespace: "test",
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&cluster).Build()
	r := &ClusterAPIClusterReconciler{
		Client:             c,
		UseClusterTopology: false,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(&cluster)})
	if err != nil {
		t.Fatal(err)
	}

	var actual clusterv1.Cluster
	err = c.Get(context.Background(), client.ObjectKeyFromObject(&cluster), &actual)
	if err != nil {
		t.Fatal(err)
	}

	if actual.Spec.InfrastructureRef == nil {
		t.Fatalf("expected infrastructure ref to be set in legacy mode")
	}

	if actual.Spec.Topology != nil {
		t.Fatalf("expected topology to remain nil in legacy mode")
	}

	var kubevirtCluster providerv1.KubevirtCluster
	err = c.Get(context.Background(), client.ObjectKeyFromObject(&cluster), &kubevirtCluster)
	if err != nil {
		t.Fatal(err)
	}

	if kubevirtCluster.Spec.ControlPlaneServiceTemplate.Spec.Type != corev1.ServiceTypeClusterIP {
		t.Fatalf("expected legacy kubevirt cluster service type %q", corev1.ServiceTypeClusterIP)
	}
}
