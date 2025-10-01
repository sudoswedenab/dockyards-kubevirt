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
	"encoding/json"
	"strconv"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	networkv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	"github.com/sudoswedenab/dockyards-backend/api/apiutil"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
)

// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=talosconfigtemplates,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datasources,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;patch;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=taloscontrolplanes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodepools/status,verbs=patch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=releases,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtmachinetemplates,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch

type DockyardsNodePoolReconciler struct {
	client.Client

	DataVolumeStorageClassName *string
	EnableMultus               bool
	ValidNodeIPSubnets         []string
}

func (r *DockyardsNodePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)

	var dockyardsNodePool dockyardsv1.NodePool
	err := r.Get(ctx, req.NamespacedName, &dockyardsNodePool)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("reconcile node pool")

	ownerCluster, err := apiutil.GetOwnerCluster(ctx, r.Client, &dockyardsNodePool)
	if err != nil {
		return ctrl.Result{}, err
	}

	if ownerCluster == nil {
		logger.Info("ignoring dockyards node pool without owner")

		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(&dockyardsNodePool, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		err := patchDockyardsNodePool(ctx, patchHelper, &dockyardsNodePool)
		if err != nil {
			result = ctrl.Result{}
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	result, err = r.reconcileMachineTemplate(ctx, &dockyardsNodePool)
	if err != nil {
		return result, err
	}

	if dockyardsNodePool.Spec.ControlPlane {
		return r.reconcileTalosControlPlane(ctx, &dockyardsNodePool, ownerCluster)
	}

	result, err = r.reconcileTalosConfigTemplate(ctx, &dockyardsNodePool, ownerCluster)
	if err != nil {
		return result, err
	}

	result, err = r.reconcileMachineDeployment(ctx, &dockyardsNodePool, ownerCluster)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsNodePoolReconciler) reconcileMachineTemplate(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	release, err := apiutil.GetDefaultRelease(ctx, r.Client, dockyardsv1.ReleaseTypeTalosInstaller)
	if err != nil {
		return ctrl.Result{}, nil
	}

	if release == nil {
		logger.Info("ignoring machine template without default release")

		return ctrl.Result{}, nil
	}

	var dataSource cdiv1.DataSource
	err = r.Get(ctx, client.ObjectKeyFromObject(release), &dataSource)
	if apierrors.IsNotFound(err) {
		conditions.MarkFalse(dockyardsNodePool, KubevirtMachineTemplateReconciledCondition, WaitingForDataSourceReason, "")

		return ctrl.Result{}, nil
	}

	if err != nil {
		return ctrl.Result{}, err
	}

	machineTemplate := providerv1.KubevirtMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &machineTemplate, func() error {
		machineTemplate.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: dockyardsv1.GroupVersion.String(),
				Kind:       dockyardsv1.NodePoolKind,
				Name:       dockyardsNodePool.Name,
				UID:        dockyardsNodePool.UID,
			},
		}

		if !machineTemplate.CreationTimestamp.IsZero() {
			return nil
		}

		machineTemplate.Spec.Template.Spec.BootstrapCheckSpec = providerv1.VirtualMachineBootstrapCheckSpec{
			CheckStrategy: "none",
		}

		cpu := dockyardsNodePool.Spec.Resources.Cpu()
		storage := dockyardsNodePool.Spec.Resources.Storage()
		memory := dockyardsNodePool.Spec.Resources.Memory()

		storageClassName := r.DataVolumeStorageClassName

		dataVolumeTemplates := []kubevirtv1.DataVolumeTemplateSpec{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "boot",
				},
				Spec: cdiv1.DataVolumeSpec{
					PVC: &corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteMany,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: *storage,
							},
						},
						StorageClassName: storageClassName,
						VolumeMode:       ptr.To(corev1.PersistentVolumeBlock),
					},
					SourceRef: &cdiv1.DataVolumeSourceRef{
						Kind:      "DataSource",
						Name:      dataSource.Name,
						Namespace: &dataSource.Namespace,
					},
				},
			},
		}

		disks := []kubevirtv1.Disk{
			{
				DiskDevice: kubevirtv1.DiskDevice{
					Disk: &kubevirtv1.DiskTarget{
						Bus: kubevirtv1.DiskBusVirtio,
					},
				},
				Name: "boot",
			},
		}

		volumes := []kubevirtv1.Volume{
			{
				VolumeSource: kubevirtv1.VolumeSource{
					DataVolume: &kubevirtv1.DataVolumeSource{
						Name: "boot",
					},
				},
				Name: "boot",
			},
		}

		for _, storageResource := range dockyardsNodePool.Spec.StorageResources {
			dataVolumeTemplate := kubevirtv1.DataVolumeTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name: storageResource.Name,
				},
				Spec: cdiv1.DataVolumeSpec{
					Source: &cdiv1.DataVolumeSource{
						Blank: &cdiv1.DataVolumeBlankImage{},
					},
					PVC: &corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteMany,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageResource.Quantity,
							},
						},
						StorageClassName: storageClassName,
						VolumeMode:       ptr.To(corev1.PersistentVolumeBlock),
					},
				},
			}

			dataVolumeTemplates = append(dataVolumeTemplates, dataVolumeTemplate)

			disk := kubevirtv1.Disk{
				DiskDevice: kubevirtv1.DiskDevice{
					Disk: &kubevirtv1.DiskTarget{
						Bus: kubevirtv1.DiskBusVirtio,
					},
				},
				Name: storageResource.Name,
			}

			disks = append(disks, disk)

			volume := kubevirtv1.Volume{
				VolumeSource: kubevirtv1.VolumeSource{
					DataVolume: &kubevirtv1.DataVolumeSource{
						Name: storageResource.Name,
					},
				},
				Name: storageResource.Name,
			}

			volumes = append(volumes, volume)
		}

		interfaces := []kubevirtv1.Interface{}
		networks := []kubevirtv1.Network{}

		defaultPodNetwork := true

		if r.EnableMultus {
			var networkAttchmentDefinitionList networkv1.NetworkAttachmentDefinitionList
			err := r.List(ctx, &networkAttchmentDefinitionList, client.InNamespace(dockyardsNodePool.Namespace))
			if err != nil {
				return err
			}

			for _, networkAttachmentDefinition := range networkAttchmentDefinitionList.Items {
				_, hasLabel := networkAttachmentDefinition.Labels[LabelNetworkAsDefault]
				if hasLabel {
					defaultPodNetwork = false
				}

				iface := kubevirtv1.Interface{
					Name:                   networkAttachmentDefinition.Name,
					InterfaceBindingMethod: kubevirtv1.DefaultBridgeNetworkInterface().InterfaceBindingMethod,
				}

				interfaces = append(interfaces, iface)

				network := kubevirtv1.Network{
					Name: networkAttachmentDefinition.Name,
					NetworkSource: kubevirtv1.NetworkSource{
						Multus: &kubevirtv1.MultusNetwork{
							NetworkName: networkAttachmentDefinition.Namespace + "/" + networkAttachmentDefinition.Name,
							Default:     hasLabel,
						},
					},
				}

				networks = append(networks, network)
			}
		}

		if defaultPodNetwork {
			interfaces = append([]kubevirtv1.Interface{*kubevirtv1.DefaultBridgeNetworkInterface()}, interfaces...)
			networks = append([]kubevirtv1.Network{*kubevirtv1.DefaultPodNetwork()}, networks...)
		}

		machineTemplate.Spec.Template.Spec.VirtualMachineTemplate.Spec = kubevirtv1.VirtualMachineSpec{
			DataVolumeTemplates: dataVolumeTemplates,
			RunStrategy:         ptr.To(kubevirtv1.RunStrategyAlways),
			Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
				Spec: kubevirtv1.VirtualMachineInstanceSpec{
					Domain: kubevirtv1.DomainSpec{
						CPU: &kubevirtv1.CPU{
							Cores: uint32(cpu.Value()),
						},
						Devices: kubevirtv1.Devices{
							Disks:      disks,
							Interfaces: interfaces,
						},
						Memory: &kubevirtv1.Memory{
							Guest: memory,
						},
					},
					EvictionStrategy: ptr.To(kubevirtv1.EvictionStrategyLiveMigrate),
					Volumes:          volumes,
					Networks:         networks,
				},
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(dockyardsNodePool, KubevirtMachineTemplateReconciledCondition, ReconciledReason, "")

	logger.Info("reconciled machine template", "result", operationResult)

	return ctrl.Result{}, nil
}

func (r *DockyardsNodePoolReconciler) reconcileSharedConfigPatches(dockyardsCluster *dockyardsv1.Cluster, configPatches []bootstrapv1.ConfigPatches) ([]bootstrapv1.ConfigPatches, error) {
	if len(dockyardsCluster.Spec.PodSubnets) > 0 {
		raw, err := json.Marshal(dockyardsCluster.Spec.PodSubnets)
		if err != nil {
			return nil, err
		}

		configPatch := bootstrapv1.ConfigPatches{
			Op:   "replace",
			Path: "/cluster/network/podSubnets",
			Value: apiextensionsv1.JSON{
				Raw: raw,
			},
		}

		configPatches = append(configPatches, configPatch)
	}

	if len(dockyardsCluster.Spec.ServiceSubnets) > 0 {
		raw, err := json.Marshal(dockyardsCluster.Spec.ServiceSubnets)
		if err != nil {
			return nil, err
		}

		configPatch := bootstrapv1.ConfigPatches{
			Op:   "replace",
			Path: "/cluster/network/serviceSubnets",
			Value: apiextensionsv1.JSON{
				Raw: raw,
			},
		}

		configPatches = append(configPatches, configPatch)
	}

	if len(r.ValidNodeIPSubnets) > 0 {
		raw, err := json.Marshal(r.ValidNodeIPSubnets)
		if err != nil {
			return nil, err
		}

		configPatch := bootstrapv1.ConfigPatches{
			Op:   "replace",
			Path: "/machine/kubelet/nodeIP/validSubnets",
			Value: apiextensionsv1.JSON{
				Raw: raw,
			},
		}

		configPatches = append(configPatches, configPatch)
	}

	return configPatches, nil
}

func (r *DockyardsNodePoolReconciler) reconcileTalosControlPlane(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	if !dockyardsCluster.Status.APIEndpoint.IsValid() {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, WaitingForClusterEndpointReason, "")

		return ctrl.Result{}, nil
	}

	talosControlPlane := controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	configPatches := []bootstrapv1.ConfigPatches{
		{
			Op:   "replace",
			Path: "/cluster/apiServer/certSANs",
			Value: apiextensionsv1.JSON{
				Raw: []byte("[" + strconv.Quote(dockyardsCluster.Status.APIEndpoint.Host) + "]"),
			},
		},
	}

	if dockyardsCluster.Spec.NoDefaultNetworkPlugin {
		raw, err := json.Marshal(map[string]string{
			"name": "none",
		})
		if err != nil {
			return ctrl.Result{}, err
		}

		configPatch := bootstrapv1.ConfigPatches{
			Op:   "replace",
			Path: "/cluster/network/cni",
			Value: apiextensionsv1.JSON{
				Raw: raw,
			},
		}

		configPatches = append(configPatches, configPatch)
	}

	configPatches, err := r.reconcileSharedConfigPatches(dockyardsCluster, configPatches)
	if err != nil {
		return ctrl.Result{}, err
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &talosControlPlane, func() error {
		talosControlPlane.Spec.Version = dockyardsCluster.Spec.Version

		if dockyardsNodePool.Spec.Replicas != nil {
			talosControlPlane.Spec.Replicas = dockyardsNodePool.Spec.Replicas
		}

		talosControlPlane.Spec.InfrastructureTemplate = corev1.ObjectReference{
			APIVersion: providerv1.GroupVersion.String(),
			Kind:       "KubevirtMachineTemplate",
			Name:       dockyardsNodePool.Name,
			Namespace:  dockyardsNodePool.Namespace,
		}

		talosControlPlane.Spec.ControlPlaneConfig = controlplanev1.ControlPlaneConfig{
			ControlPlaneConfig: bootstrapv1.TalosConfigSpec{
				GenerateType:  "controlplane",
				TalosVersion:  "v1.7",
				ConfigPatches: configPatches,
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled talos control plane", "result", operationResult)

	conditions.MarkTrue(dockyardsNodePool, TalosControlPlaneReconciledCondition, ReconciledReason, "")

	var cluster clusterv1.Cluster
	err = r.Get(ctx, client.ObjectKeyFromObject(dockyardsCluster), &cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cluster.Spec.ControlPlaneRef == nil {
		patch := client.MergeFrom(cluster.DeepCopy())

		cluster.Spec.ControlPlaneRef = &corev1.ObjectReference{
			APIVersion: controlplanev1.GroupVersion.String(),
			Kind:       "TalosControlPlane",
			Name:       talosControlPlane.Name,
		}

		err := r.Patch(ctx, &cluster, patch)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsNodePoolReconciler) reconcileTalosConfigTemplate(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	talosConfigTemplate := bootstrapv1.TalosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	configPatches, err := r.reconcileSharedConfigPatches(dockyardsCluster, []bootstrapv1.ConfigPatches{})
	if err != nil {
		return ctrl.Result{}, err
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &talosConfigTemplate, func() error {
		talosConfigTemplate.Spec.Template.Spec.GenerateType = "worker"
		talosConfigTemplate.Spec.Template.Spec.TalosVersion = "v1.7"

		talosConfigTemplate.Spec.Template.Spec.ConfigPatches = configPatches

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(dockyardsNodePool, TalosConfigTemplateReconciledCondition, ReconciledReason, "")

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled talos config template", "result", operationResult)
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsNodePoolReconciler) reconcileMachineDeployment(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	machineDeployment := clusterv1.MachineDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &machineDeployment, func() error {
		if dockyardsNodePool.Spec.Replicas != nil {
			machineDeployment.Spec.Replicas = dockyardsNodePool.Spec.Replicas
		}

		machineDeployment.Spec.ClusterName = dockyardsCluster.Name
		machineDeployment.Spec.Template.Spec.ClusterName = dockyardsCluster.Name
		machineDeployment.Spec.Template.Spec.Version = &dockyardsCluster.Spec.Version

		machineDeployment.Spec.Template.Spec.Bootstrap = clusterv1.Bootstrap{
			ConfigRef: &corev1.ObjectReference{
				APIVersion: bootstrapv1.GroupVersion.String(),
				Kind:       "TalosConfigTemplate",
				Name:       dockyardsNodePool.Name,
			},
		}

		machineDeployment.Spec.Template.Spec.InfrastructureRef = corev1.ObjectReference{
			APIVersion: providerv1.GroupVersion.String(),
			Kind:       "KubevirtMachineTemplate",
			Name:       dockyardsNodePool.Name,
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(dockyardsNodePool, MachineDeploymentReconciledCondition, ReconciledReason, "")

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled machine deployment", "result", operationResult)
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsNodePoolReconciler) dockyardsClusterToDockyardsNodePools(ctx context.Context, obj client.Object) []ctrl.Request {
	cluster, ok := obj.(*dockyardsv1.Cluster)
	if !ok {
		return nil
	}

	matchingLabels := client.MatchingLabels{
		dockyardsv1.LabelClusterName: cluster.Name,
	}

	var nodePoolList dockyardsv1.NodePoolList
	err := r.List(ctx, &nodePoolList, matchingLabels, client.InNamespace(cluster.Namespace))
	if err != nil {
		return nil
	}

	requests := []ctrl.Request{}

	for _, item := range nodePoolList.Items {
		requests = append(requests, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Namespace: item.Namespace,
				Name:      item.Name,
			},
		})
	}

	return requests
}

func (r *DockyardsNodePoolReconciler) SetupWithManager(m ctrl.Manager) error {
	scheme := m.GetScheme()

	_ = bootstrapv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)

	if r.EnableMultus {
		_ = networkv1.AddToScheme(scheme)
	}

	err := ctrl.NewControllerManagedBy(m).
		For(&dockyardsv1.NodePool{}).
		Watches(
			&dockyardsv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.dockyardsClusterToDockyardsNodePools),
		).
		Complete(r)
	if err != nil {
		return err
	}

	return nil
}

func patchDockyardsNodePool(ctx context.Context, patchHelper *patch.Helper, dockyardsNodePool *dockyardsv1.NodePool, opts ...patch.Option) error {
	summaryConditions := []string{
		KubevirtMachineTemplateReconciledCondition,
	}

	if dockyardsNodePool.Spec.ControlPlane {
		summaryConditions = append(
			summaryConditions,
			TalosControlPlaneReconciledCondition,
		)
	} else {
		summaryConditions = append(
			summaryConditions,
			TalosConfigTemplateReconciledCondition,
			MachineDeploymentReconciledCondition,
		)
	}

	conditions.SetSummary(
		dockyardsNodePool,
		dockyardsv1.ReadyCondition,
		conditions.WithConditions(summaryConditions...),
	)

	return patchHelper.Patch(ctx, dockyardsNodePool, opts...)
}
