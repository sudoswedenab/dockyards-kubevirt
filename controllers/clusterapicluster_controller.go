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

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;patch;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusterclasses,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters;nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtclusters,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtclustertemplates,verbs=create;get;list;patch;watch

type ClusterAPIClusterReconciler struct {
	client.Client

	UseClusterTopology bool
}

func (r *ClusterAPIClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	var cluster clusterv1.Cluster
	err := r.Get(ctx, req.NamespacedName, &cluster)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(&cluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		err := patchHelper.Patch(ctx, &cluster)
		if err != nil {
			result = ctrl.Result{}
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	if r.UseClusterTopology {
		result, err = r.reconcileKubevirtCluster(ctx, &cluster)
		if err != nil {
			return result, err
		}

		result, err = r.reconcileClusterClassTopology(ctx, &cluster)
	} else {
		result, err = r.reconcileKubevirtCluster(ctx, &cluster)
	}
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *ClusterAPIClusterReconciler) reconcileClusterClassTopology(ctx context.Context, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	var dockyardsCluster dockyardsv1.Cluster
	err := r.Get(ctx, client.ObjectKeyFromObject(cluster), &dockyardsCluster)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	matchingLabels := client.MatchingLabels{
		dockyardsv1.LabelClusterName: cluster.Name,
	}

	var nodePoolList dockyardsv1.NodePoolList
	err = r.List(ctx, &nodePoolList, matchingLabels, client.InNamespace(cluster.Namespace))
	if err != nil {
		return ctrl.Result{}, err
	}

	var controlPlaneNodePool *dockyardsv1.NodePool
	workerNodePools := make([]dockyardsv1.NodePool, 0, len(nodePoolList.Items))

	for i := range nodePoolList.Items {
		nodePool := nodePoolList.Items[i]

		if nodePool.Spec.ControlPlane {
			if controlPlaneNodePool == nil {
				controlPlaneNodePool = &nodePool
			}

			continue
		}

		workerNodePools = append(workerNodePools, nodePool)
	}

	if controlPlaneNodePool == nil {
		logger.Info("ignoring cluster topology reconciliation without a control plane node pool")

		return ctrl.Result{}, nil
	}

	kubevirtClusterTemplate := providerv1.KubevirtClusterTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + "-cluster-template",
			Namespace: cluster.Namespace,
		},
	}

	_, err = controllerutil.CreateOrPatch(ctx, r.Client, &kubevirtClusterTemplate, func() error {
		kubevirtClusterTemplate.Spec.Template.Spec.ControlPlaneServiceTemplate = providerv1.ControlPlaneServiceTemplate{
			Spec: providerv1.ServiceSpecTemplate{
				Type: corev1.ServiceTypeLoadBalancer,
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	clusterClass := clusterv1.ClusterClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name + "-class",
			Namespace: cluster.Namespace,
		},
	}

	_, err = controllerutil.CreateOrPatch(ctx, r.Client, &clusterClass, func() error {
		clusterClass.Spec.Infrastructure = clusterv1.LocalObjectTemplate{
			Ref: &corev1.ObjectReference{
				APIVersion: providerv1.GroupVersion.String(),
				Kind:       "KubevirtClusterTemplate",
				Name:       kubevirtClusterTemplate.Name,
			},
		}

		clusterClass.Spec.ControlPlane = clusterv1.ControlPlaneClass{
			LocalObjectTemplate: clusterv1.LocalObjectTemplate{
				Ref: &corev1.ObjectReference{
					APIVersion: "controlplane.cluster.x-k8s.io/v1alpha3",
					Kind:       "TalosControlPlane",
					Name:       controlPlaneNodePool.Name,
				},
			},
			MachineInfrastructure: &clusterv1.LocalObjectTemplate{
				Ref: &corev1.ObjectReference{
					APIVersion: providerv1.GroupVersion.String(),
					Kind:       "KubevirtMachineTemplate",
					Name:       controlPlaneNodePool.Name,
				},
			},
		}

		clusterClass.Spec.Workers = clusterv1.WorkersClass{}
		clusterClass.Spec.Workers.MachineDeployments = make([]clusterv1.MachineDeploymentClass, 0, len(workerNodePools))

		for _, nodePool := range workerNodePools {
			talosConfigTemplateName := nodePool.Name
			if value, ok := nodePool.Annotations[AnnotationTalosConfigTemplateName]; ok && value != "" {
				talosConfigTemplateName = value
			}

			clusterClass.Spec.Workers.MachineDeployments = append(clusterClass.Spec.Workers.MachineDeployments, clusterv1.MachineDeploymentClass{
				Class: nodePool.Name,
				Template: clusterv1.MachineDeploymentClassTemplate{
					Bootstrap: clusterv1.LocalObjectTemplate{
						Ref: &corev1.ObjectReference{
							APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha3",
							Kind:       "TalosConfigTemplate",
							Name:       talosConfigTemplateName,
						},
					},
					Infrastructure: clusterv1.LocalObjectTemplate{
						Ref: &corev1.ObjectReference{
							APIVersion: providerv1.GroupVersion.String(),
							Kind:       "KubevirtMachineTemplate",
							Name:       nodePool.Name,
						},
					},
				},
			})
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	cluster.Spec.Topology = &clusterv1.Topology{
		Class:   clusterClass.Name,
		Version: dockyardsCluster.Spec.Version,
		ControlPlane: clusterv1.ControlPlaneTopology{
			Replicas: controlPlaneNodePool.Spec.Replicas,
		},
	}

	if len(workerNodePools) > 0 {
		cluster.Spec.Topology.Workers = &clusterv1.WorkersTopology{}
		cluster.Spec.Topology.Workers.MachineDeployments = make([]clusterv1.MachineDeploymentTopology, 0, len(workerNodePools))

		for _, nodePool := range workerNodePools {
			cluster.Spec.Topology.Workers.MachineDeployments = append(cluster.Spec.Topology.Workers.MachineDeployments, clusterv1.MachineDeploymentTopology{
				Class:    nodePool.Name,
				Name:     nodePool.Name,
				Replicas: nodePool.Spec.Replicas,
			})
		}
	}

	logger.Info("reconciled cluster topology and class", "clusterClass", clusterClass.Name)

	return ctrl.Result{}, nil
}

func (r *ClusterAPIClusterReconciler) dockyardsClusterToCAPICluster(_ context.Context, obj client.Object) []ctrl.Request {
	cluster, ok := obj.(*dockyardsv1.Cluster)
	if !ok {
		return nil
	}

	return []ctrl.Request{{NamespacedName: client.ObjectKeyFromObject(cluster)}}
}

func (r *ClusterAPIClusterReconciler) dockyardsNodePoolToCAPICluster(_ context.Context, obj client.Object) []ctrl.Request {
	nodePool, ok := obj.(*dockyardsv1.NodePool)
	if !ok {
		return nil
	}

	clusterName, hasClusterName := nodePool.Labels[dockyardsv1.LabelClusterName]
	if !hasClusterName {
		return nil
	}

	return []ctrl.Request{{
		NamespacedName: client.ObjectKey{
			Namespace: nodePool.Namespace,
			Name:      clusterName,
		},
	}}
}

func (r *ClusterAPIClusterReconciler) reconcileKubevirtCluster(ctx context.Context, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	kubevirtCluster := providerv1.KubevirtCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &kubevirtCluster, func() error {
		kubevirtCluster.Spec.ControlPlaneServiceTemplate = providerv1.ControlPlaneServiceTemplate{
			Spec: providerv1.ServiceSpecTemplate{
				Type: corev1.ServiceTypeClusterIP,
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled kubevirt cluster", "result", operationResult)
	}

	if cluster.Spec.InfrastructureRef == nil {
		cluster.Spec.InfrastructureRef = &corev1.ObjectReference{
			APIVersion: providerv1.GroupVersion.String(),
			Kind:       "KubevirtCluster",
			Name:       kubevirtCluster.Name,
			Namespace:  kubevirtCluster.Namespace,
		}
	}

	return ctrl.Result{}, nil
}

func (r *ClusterAPIClusterReconciler) SetupWithManager(m ctrl.Manager) error {
	scheme := m.GetScheme()

	_ = clusterv1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(m).
		For(&clusterv1.Cluster{}).
		Watches(
			&dockyardsv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.dockyardsClusterToCAPICluster),
		).
		Watches(
			&dockyardsv1.NodePool{},
			handler.EnqueueRequestsFromMapFunc(r.dockyardsNodePoolToCAPICluster),
		).
		Complete(r)
	if err != nil {
		return err
	}

	return nil
}
