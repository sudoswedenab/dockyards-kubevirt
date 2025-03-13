package controllers

import (
	"context"
	"encoding/json"

	"bitbucket.org/sudosweden/dockyards-backend/pkg/api/apiutil"
	dockyardsv1 "bitbucket.org/sudosweden/dockyards-backend/pkg/api/v1alpha3"
	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=talosconfigtemplates,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;patch;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=taloscontrolplanes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodepools/status,verbs=patch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=releases,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtmachinetemplates,verbs=create;get;list;patch;watch

type DockyardsNodePoolReconciler struct {
	client.Client

	DataVolumeStorageClassName *string
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

	result, err = r.reconcileTalosConfigTemplate(ctx, &dockyardsNodePool)
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

	var dataVolume cdiv1.DataVolume
	err = r.Get(ctx, client.ObjectKeyFromObject(release), &dataVolume)
	if apierrors.IsNotFound(err) {
		conditions.MarkFalse(dockyardsNodePool, KubevirtMachineTemplateReconciledCondition, WaitingForDataVolumeReason, "")

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
					Source: &cdiv1.DataVolumeSource{
						PVC: &cdiv1.DataVolumeSourcePVC{
							Name:      dataVolume.Status.ClaimName,
							Namespace: dataVolume.Namespace,
						},
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
							Disks: disks,
							Interfaces: []kubevirtv1.Interface{
								{
									Name: "default",
									InterfaceBindingMethod: kubevirtv1.InterfaceBindingMethod{
										Bridge: &kubevirtv1.InterfaceBridge{},
									},
								},
							},
						},
						Memory: &kubevirtv1.Memory{
							Guest: memory,
						},
					},
					EvictionStrategy: ptr.To(kubevirtv1.EvictionStrategyLiveMigrate),
					Volumes:          volumes,
					Networks: []kubevirtv1.Network{
						{
							Name: "default",
							NetworkSource: kubevirtv1.NetworkSource{
								Pod: &kubevirtv1.PodNetwork{},
							},
						},
					},
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

func (r *DockyardsNodePoolReconciler) reconcileTalosControlPlane(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	talosControlPlane := controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	var tlsRoute gatewayapiv1alpha2.TLSRoute
	err := r.Get(ctx, client.ObjectKeyFromObject(dockyardsCluster), &tlsRoute)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if apierrors.IsNotFound(err) {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, WaitingForTLSRouteReason, "")

		return ctrl.Result{}, nil
	}

	certSANs, err := json.Marshal(tlsRoute.Spec.Hostnames)
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
				GenerateType: "controlplane",
				TalosVersion: "v1.7",
				ConfigPatches: []bootstrapv1.ConfigPatches{
					{
						Op:   "replace",
						Path: "/cluster/network/podSubnets",
						Value: apiextensionsv1.JSON{
							Raw: []byte("[\"10.128.0.0/16\"]"),
						},
					},
					{
						Op:   "replace",
						Path: "/cluster/network/serviceSubnets",
						Value: apiextensionsv1.JSON{
							Raw: []byte("[\"10.112.0.0/12\"]"),
						},
					},
					{
						Op:   "replace",
						Path: "/cluster/apiServer/certSANs",
						Value: apiextensionsv1.JSON{
							Raw: certSANs,
						},
					},
				},
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

func (r *DockyardsNodePoolReconciler) reconcileTalosConfigTemplate(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	talosConfigTemplate := bootstrapv1.TalosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &talosConfigTemplate, func() error {
		talosConfigTemplate.Spec.Template.Spec.GenerateType = "worker"
		talosConfigTemplate.Spec.Template.Spec.TalosVersion = "v1.7"

		talosConfigTemplate.Spec.Template.Spec.ConfigPatches = []bootstrapv1.ConfigPatches{
			{
				Op:   "replace",
				Path: "/cluster/network/podSubnets",
				Value: apiextensionsv1.JSON{
					Raw: []byte("[\"10.128.0.0/16\"]"),
				},
			},
			{
				Op:   "replace",
				Path: "/cluster/network/serviceSubnets",
				Value: apiextensionsv1.JSON{
					Raw: []byte("[\"10.112.0.0/12\"]"),
				},
			},
		}

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

func (r *DockyardsNodePoolReconciler) SetupWithManager(m ctrl.Manager) error {
	scheme := m.GetScheme()

	_ = bootstrapv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)
	_ = gatewayapiv1alpha2.Install(scheme)

	err := ctrl.NewControllerManagedBy(m).For(&dockyardsv1.NodePool{}).Complete(r)
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
