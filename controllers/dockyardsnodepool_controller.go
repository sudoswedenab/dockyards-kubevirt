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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	networkv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	"github.com/sudoswedenab/dockyards-backend/api/apiutil"
	dyconfig "github.com/sudoswedenab/dockyards-backend/api/config"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	apiserverv1 "k8s.io/apiserver/pkg/apis/apiserver/v1beta1"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	sigsyaml "sigs.k8s.io/yaml"

	talospatchv1 "github.com/sudoswedenab/dockyards-kubevirt/internal/talospatch/v1alpha1"
)

// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=talosconfigtemplates,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datasources,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=taloscontrolplanes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodepools/status,verbs=patch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodepools,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodeclasses,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=releases,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtmachinetemplates,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch

type StrategicPatches []string

type DockyardsNodePoolReconciler struct {
	client.Client

	TalosClusterDiscoveryServiceEndpoint string
	DataVolumeStorageClassName           *string
	EnableMultus                         bool
	ValidNodeIPSubnets                   []string
	UseBlockStorage                      bool
	DockyardsConfig                      *dyconfig.ConfigManager
	NetworkInterfaceMultiQueue           bool
}

const (
	defaultTalosInstallerDataVolumeSize  = "8Gi"
	clusterNetworkInterfaceMultiqueueKey = "networkInterfaceMultiqueue"
)

type talosInstallerOverride struct {
	URL  string
	Size resource.Quantity
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

	if ownerCluster.Name == "" {
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
		return r.reconcileTalosControlPlane(ctx, &dockyardsNodePool, &ownerCluster)
	}

	result, err = r.reconcileTalosConfigTemplate(ctx, &dockyardsNodePool, &ownerCluster)
	if err != nil {
		return result, err
	}

	result, err = r.reconcileMachineDeployment(ctx, &dockyardsNodePool, &ownerCluster)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsNodePoolReconciler) reconcileMachineTemplate(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	publicNamespace := r.DockyardsConfig.GetValueOrDefault(dyconfig.KeyPublicNamespace, "dockyards-public")

	var dataSource cdiv1.DataSource
	ownerCluster, customTalosInstaller, err := r.resolveTalosInstallerOverride(ctx, dockyardsNodePool)
	if err != nil {
		return ctrl.Result{}, err
	}

	if customTalosInstaller != nil && ownerCluster != nil {
		dataSource, err = r.reconcileCustomTalosInstallerDataSource(ctx, ownerCluster, dockyardsNodePool, *customTalosInstaller)
		if err != nil {
			return ctrl.Result{}, err
		}
	} else {
		release, err := apiutil.GetDefaultRelease(ctx, r.Client, dockyardsv1.ReleaseTypeTalosInstaller)
		if err != nil {
			return ctrl.Result{}, nil
		}

		if release == nil {
			logger.Info("ignoring machine template without default release")

			return ctrl.Result{}, nil
		}

		err = r.Get(ctx, client.ObjectKeyFromObject(release), &dataSource)
		if apierrors.IsNotFound(err) {
			conditions.MarkFalse(dockyardsNodePool, KubevirtMachineTemplateReconciledCondition, WaitingForDataSourceReason, "")

			return ctrl.Result{}, nil
		}

		if err != nil {
			return ctrl.Result{}, err
		}
	}

	storageClassName, err := r.resolveDataVolumeStorageClassName(ctx, dockyardsNodePool)
	if err != nil {
		return ctrl.Result{}, err
	}

	networkInterfaceMultiqueue, err := r.resolveNetworkInterfaceMultiqueue(ctx, dockyardsNodePool)
	if err != nil {
		return ctrl.Result{}, err
	}

	var nodeClass *dockyardsv1.NodeClass
	if dockyardsNodePool.Spec.NodeClassRef != nil {
		logger.Info("looking up NodeClass for NodePool", "nodePool", dockyardsNodePool.Name, "nodeClass", dockyardsNodePool.Spec.NodeClassRef.Name)

		nodeClass = &dockyardsv1.NodeClass{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dockyardsNodePool.Spec.NodeClassRef.Name,
				Namespace: publicNamespace,
			},
		}

		err = r.Get(ctx, client.ObjectKeyFromObject(nodeClass), nodeClass)
		if err != nil {
			return ctrl.Result{}, err
		}
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
					},
					SourceRef: &cdiv1.DataVolumeSourceRef{
						Kind:      "DataSource",
						Name:      dataSource.Name,
						Namespace: &dataSource.Namespace,
					},
				},
			},
		}

		if r.UseBlockStorage {
			for _, dvt := range dataVolumeTemplates {
				dvt.Spec.PVC.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
			}
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
					},
				},
			}

			if r.UseBlockStorage {
				dataVolumeTemplate.Spec.PVC.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
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
							Disks:                      disks,
							Interfaces:                 interfaces,
							NetworkInterfaceMultiQueue: networkInterfaceMultiqueue,
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

		if nodeClass != nil {
			if nodeClass.Spec.NodeSelector != nil {
				machineTemplate.Spec.Template.Spec.VirtualMachineTemplate.Spec.Template.Spec.NodeSelector = nodeClass.Spec.NodeSelector
			}

			if nodeClass.Spec.NodeAffinity != nil {
				if machineTemplate.Spec.Template.Spec.VirtualMachineTemplate.Spec.Template.Spec.Affinity == nil {
					machineTemplate.Spec.Template.Spec.VirtualMachineTemplate.Spec.Template.Spec.Affinity = &corev1.Affinity{}
				}

				machineTemplate.Spec.Template.Spec.VirtualMachineTemplate.Spec.Template.Spec.Affinity.NodeAffinity = nodeClass.Spec.NodeAffinity
			}

			if nodeClass.Spec.Tolerations != nil {
				machineTemplate.Spec.Template.Spec.VirtualMachineTemplate.Spec.Template.Spec.Tolerations = nodeClass.Spec.Tolerations
			}
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

func (r *DockyardsNodePoolReconciler) resolveTalosInstallerOverride(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool) (*dockyardsv1.Cluster, *talosInstallerOverride, error) {
	ownerCluster, err := apiutil.GetOwnerCluster(ctx, r.Client, dockyardsNodePool)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, err
	}

	if ownerCluster.Name == "" {
		return nil, nil, nil
	}

	customTalosInstallerURL := ownerCluster.Spec.Advanced.Kubevirt.Talos.InstallImage.URL

	customTalosInstallerURL = strings.TrimSpace(customTalosInstallerURL)
	if customTalosInstallerURL == "" {
		return &ownerCluster, nil, nil
	}

	talosInstallerSizeRaw := ownerCluster.Spec.Advanced.Kubevirt.Talos.InstallImage.Size

	talosInstallerSizeRaw = strings.TrimSpace(talosInstallerSizeRaw)
	if talosInstallerSizeRaw == "" {
		talosInstallerSizeRaw = defaultTalosInstallerDataVolumeSize
	}

	talosInstallerSize, err := resource.ParseQuantity(talosInstallerSizeRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid talos installer size %q: %w", talosInstallerSizeRaw, err)
	}

	return &ownerCluster, &talosInstallerOverride{
		URL:  customTalosInstallerURL,
		Size: talosInstallerSize,
	}, nil
}

func (r *DockyardsNodePoolReconciler) reconcileCustomTalosInstallerDataSource(
	ctx context.Context,
	ownerCluster *dockyardsv1.Cluster,
	dockyardsNodePool *dockyardsv1.NodePool,
	talosInstaller talosInstallerOverride,
) (cdiv1.DataSource, error) {
	storageClassName, err := r.resolveDataVolumeStorageClassName(ctx, dockyardsNodePool)
	if err != nil {
		return cdiv1.DataSource{}, err
	}

	dataVolumeName := clusterTalosInstallerDataVolumeName(ownerCluster.Name, talosInstaller)
	dataVolume := cdiv1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataVolumeName,
			Namespace: ownerCluster.Namespace,
		},
	}

	_, err = controllerutil.CreateOrPatch(ctx, r.Client, &dataVolume, func() error {
		dataVolume.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: dockyardsv1.GroupVersion.String(),
				Kind:       dockyardsv1.ClusterKind,
				Name:       ownerCluster.Name,
				UID:        ownerCluster.UID,
			},
		}

		if dataVolume.Annotations == nil {
			dataVolume.Annotations = make(map[string]string)
		}

		dataVolume.Annotations["cdi.kubevirt.io/storage.bind.immediate.requested"] = ""

		if dataVolume.Labels == nil {
			dataVolume.Labels = make(map[string]string)
		}

		dataVolume.Labels[dockyardsv1.LabelClusterName] = ownerCluster.Name

		dataVolume.Spec.Storage = &cdiv1.StorageSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: talosInstaller.Size,
				},
			},
		}

		if r.UseBlockStorage {
			dataVolume.Spec.Storage.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
		}

		if storageClassName != nil {
			dataVolume.Spec.Storage.StorageClassName = storageClassName
		}

		dataVolume.Spec.Source = &cdiv1.DataVolumeSource{
			HTTP: &cdiv1.DataVolumeSourceHTTP{
				URL: talosInstaller.URL,
			},
		}

		return nil
	})
	if err != nil {
		return cdiv1.DataSource{}, err
	}

	dataSource := cdiv1.DataSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterTalosInstallerDataSourceName(ownerCluster.Name),
			Namespace: ownerCluster.Namespace,
		},
	}

	_, err = controllerutil.CreateOrPatch(ctx, r.Client, &dataSource, func() error {
		if dataSource.Labels == nil {
			dataSource.Labels = make(map[string]string)
		}

		dataSource.Labels[dockyardsv1.LabelClusterName] = ownerCluster.Name

		dataSource.Spec.Source.PVC = &cdiv1.DataVolumeSourcePVC{
			Name:      dataVolume.Name,
			Namespace: dataVolume.Namespace,
		}

		return nil
	})
	if err != nil {
		return cdiv1.DataSource{}, err
	}

	return dataSource, nil
}

func clusterTalosInstallerDataSourceName(clusterName string) string {
	return cappedKubernetesName(clusterName, "-talos-installer")
}

func clusterTalosInstallerDataVolumeName(clusterName string, talosInstaller talosInstallerOverride) string {
	hashInput := talosInstaller.URL + "|" + talosInstaller.Size.String()
	checksum := sha256.Sum256([]byte(hashInput))
	suffix := "-talos-installer-" + hex.EncodeToString(checksum[:6])

	return cappedKubernetesName(clusterName, suffix)
}

func cappedKubernetesName(base, suffix string) string {
	if len(base)+len(suffix) <= 253 {
		return base + suffix
	}

	maxBaseLength := 253 - len(suffix)
	if maxBaseLength < 0 {
		maxBaseLength = 0
	}

	return base[:maxBaseLength] + suffix
}

func (r *DockyardsNodePoolReconciler) talosConfigPatch(dockyardsCluster *dockyardsv1.Cluster) talospatchv1.Config {
	// This is the patches we apply to the main talos config
	// The file look something like this:
	//
	// version: v1alpha1
	// cluster:
	//   network:
	//     podSubnets:
	//       - 1.2.3.4
	//     serviceSubnets:
	//       - 1.2.3.4
	//     cni:
	//       name: foobar
	//   apiServer:
	//     certSANs:
	//       - talos-api.example.com
	//   etcd:
	//     advertisedSubnets:
	//       - 1.2.3.4
	//     listenSubnets:
	//       - 1.2.3.4
	//   discovery:
	//     registries:
	//       service:
	//         endpoint: "discovery-service.example.com"
	// machine:
	//   env:
	//     some_key: some_value
	//   kubelet:
	//     nodeIP:
	//       validSubnets:
	//         - 1.2.3.4

	patch := talospatchv1.Config{
		Version: talospatchv1.ConfigVersion,
	}

	if len(dockyardsCluster.Spec.PodSubnets) > 0 {
		patch.Cluster.Network.PodSubnets = dockyardsCluster.Spec.PodSubnets
	}

	if len(dockyardsCluster.Spec.ServiceSubnets) > 0 {
		patch.Cluster.Network.ServiceSubnets = dockyardsCluster.Spec.ServiceSubnets
	}

	if len(r.ValidNodeIPSubnets) > 0 {
		patch.Machine.Kubelet.NodeIP.ValidSubnets = r.ValidNodeIPSubnets
	}

	value, found := r.DockyardsConfig.GetValueForKey(KeyNoProxy)
	if found {
		patch.Machine.Env.Set("no_proxy", value)
	}

	value, found = r.DockyardsConfig.GetValueForKey(KeyHttpProxy)
	if found {
		patch.Machine.Env.Set("http_proxy", value)
	}

	value, found = r.DockyardsConfig.GetValueForKey(KeyHttpsProxy)
	if found {
		patch.Machine.Env.Set("https_proxy", value)
	}

	if r.TalosClusterDiscoveryServiceEndpoint == "0" {
		patch.Cluster.Discovery.Registries.Service.Disabled = ptr.To(true)
	} else {
		patch.Cluster.Discovery.Registries.Service.Endpoint = r.TalosClusterDiscoveryServiceEndpoint
	}

	return patch
}

func (r *DockyardsNodePoolReconciler) resolveDataVolumeStorageClassName(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool) (*string, error) {
	ownerCluster, err := apiutil.GetOwnerCluster(ctx, r.Client, dockyardsNodePool)
	if apierrors.IsNotFound(err) {
		return r.DataVolumeStorageClassName, nil
	}

	if err != nil {
		return nil, err
	}

	if ownerCluster.Name == "" {
		return r.DataVolumeStorageClassName, nil
	}

	clusterStorageClassName := ownerCluster.Spec.Advanced.Kubevirt.DataVolumeStorageClassName

	clusterStorageClassName = strings.TrimSpace(clusterStorageClassName)
	if clusterStorageClassName != "" {
		return ptr.To(clusterStorageClassName), nil
	}

	return r.DataVolumeStorageClassName, nil
}

func (r *DockyardsNodePoolReconciler) resolveNetworkInterfaceMultiqueue(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool) (*bool, error) {
	ownerCluster, err := apiutil.GetOwnerCluster(ctx, r.Client, dockyardsNodePool)
	if apierrors.IsNotFound(err) {
		return ptr.To(r.NetworkInterfaceMultiQueue), nil
	}

	if err != nil {
		return nil, err
	}

	if ownerCluster.Name == "" {
		return ptr.To(r.NetworkInterfaceMultiQueue), nil
	}

	unstructuredDockyardsCluster := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": dockyardsv1.GroupVersion.String(),
			"kind":       dockyardsv1.ClusterKind,
			"metadata": map[string]any{
				"name":      ownerCluster.Name,
				"namespace": ownerCluster.Namespace,
			},
		},
	}

	err = r.Get(ctx, client.ObjectKeyFromObject(&unstructuredDockyardsCluster), &unstructuredDockyardsCluster)
	if err != nil {
		return nil, err
	}

	enabled, found, err := unstructured.NestedBool(unstructuredDockyardsCluster.Object, "spec", "advanced", "kubevirt", clusterNetworkInterfaceMultiqueueKey)
	if err != nil {
		return nil, err
	}

	if !found {
		return ptr.To(r.NetworkInterfaceMultiQueue), nil
	}

	return ptr.To(enabled), nil
}

func (r *DockyardsNodePoolReconciler) timeSyncConfigPatch() talospatchv1.TimeSyncConfig {
	// Configure NTP servers using the Talos TimeSyncConfig document (Talos v1.12+).
	//
	// Example:
	// apiVersion: v1alpha1
	// kind: TimeSyncConfig
	// ntp:
	//   servers:
	//     - time.cloudflare.com
	//
	// PTP can be configured as well:
	// apiVersion: v1alpha1
	// kind: TimeSyncConfig
	// ptp:
	//   devices:
	//     - eth0

	patch := talospatchv1.TimeSyncConfig{
		Meta: talospatchv1.Meta{
			APIVersion: talospatchv1.TimeSyncConfigAPIVersion,
			Kind:       talospatchv1.TimeSyncConfigKind,
		},
	}

	if value, found := r.DockyardsConfig.GetValueForKey(KeyNtpServers); found {
		patch.NTP.Servers = parseCommaSeparatedUnique(value)
	}

	if value, found := r.DockyardsConfig.GetValueForKey(KeyPtpDevices); found {
		patch.PTP.Devices = parseCommaSeparatedUnique(value)
	}

	return patch
}

func (r *DockyardsNodePoolReconciler) labelConfigPatch(labels map[string]string) talospatchv1.Config {
	return talospatchv1.Config{
		Version: talospatchv1.ConfigVersion,
		Machine: talospatchv1.MachineConfig{
			NodeLabels: labels,
		},
	}
}

func (r *DockyardsNodePoolReconciler) taintConfigPatch(taints map[string]string) talospatchv1.Config {
	return talospatchv1.Config{
		Version: talospatchv1.ConfigVersion,
		Machine: talospatchv1.MachineConfig{
			NodeTaints: taints,
		},
	}
}

func (r *DockyardsNodePoolReconciler) addNodePoolNodeLabelsConfigPatch(dockyardsNodePool *dockyardsv1.NodePool, strategicPatches *StrategicPatches) error {
	labels := dockyardsNodePool.Spec.NodeLabels
	if len(labels) == 0 {
		return nil
	}

	patch := r.labelConfigPatch(labels)
	err := strategicPatches.Add(new(patch))
	if err != nil {
		return fmt.Errorf("could not add node labels strategic patch: %w", err)
	}

	return nil
}

func (r *DockyardsNodePoolReconciler) addNodePoolNodeTaintsConfigPatch(dockyardsNodePool *dockyardsv1.NodePool, strategicPatches *StrategicPatches) error {
	taints := dockyardsNodePool.Spec.NodeTaints
	if len(taints) == 0 {
		return nil
	}

	for key, value := range taints {
		value = strings.TrimSpace(value)

		splitIndex := strings.LastIndex(value, ":")
		if splitIndex == -1 {
			return fmt.Errorf("spec.nodeTaints.%s must be formatted as <value>:<effect>", key)
		}

		effect := strings.TrimSpace(value[splitIndex+1:])
		if effect == "" {
			return fmt.Errorf("spec.nodeTaints.%s effect is required", key)
		}

		switch corev1.TaintEffect(effect) {
		case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
		default:
			return fmt.Errorf("spec.nodeTaints.%s.effect %q is invalid", key, effect)
		}

		taints[key] = value
	}

	patch := r.taintConfigPatch(taints)
	err := strategicPatches.Add(new(patch))
	if err != nil {
		return fmt.Errorf("could not add node taints strategic patch: %w", err)
	}

	return nil
}

func (r *DockyardsNodePoolReconciler) addSharedConfigPatches(
	dockyardsCluster *dockyardsv1.Cluster,
	strategicPatches *StrategicPatches,
) error {
	err := strategicPatches.Add(ptr.To(r.talosConfigPatch(dockyardsCluster)))
	if err != nil {
		return fmt.Errorf("could not add talos config patches: %w", err)
	}

	err = strategicPatches.Add(ptr.To(r.timeSyncConfigPatch()))
	if err != nil {
		return fmt.Errorf("could not add time sync config patches: %w", err)
	}

	patches := dockyardsCluster.Spec.Advanced.Kubevirt.Talos.AdditionalSharedConfigPatches
	if len(patches) > 0 {
		err = strategicPatches.AddMany(patches)
		if err != nil {
			return err
		}

	}

	return nil
}

func (r *DockyardsNodePoolReconciler) reconcileTalosControlPlane(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	if !dockyardsCluster.Status.APIEndpoint.IsValid() {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, WaitingForClusterEndpointReason, "")

		return ctrl.Result{}, nil
	}

	var strategicPatches StrategicPatches

	err := r.addSharedConfigPatches(dockyardsCluster, &strategicPatches)
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, nil
	}

	err = r.addNodePoolNodeLabelsConfigPatch(dockyardsNodePool, &strategicPatches)
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, nil
	}

	err = r.addNodePoolNodeTaintsConfigPatch(dockyardsNodePool, &strategicPatches)
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, err
	}

	controlPlanePatch := talospatchv1.Config{
		Version: talospatchv1.ConfigVersion,
	}
	if dockyardsCluster.Status.APIEndpoint.Host != "" {
		controlPlanePatch.Cluster.APIServer.CertSANs = []string{dockyardsCluster.Status.APIEndpoint.Host}
	}

	if len(r.ValidNodeIPSubnets) > 0 {
		controlPlanePatch.Cluster.ETCD.AdvertisedSubnets = r.ValidNodeIPSubnets
		controlPlanePatch.Cluster.ETCD.ListenSubnets = r.ValidNodeIPSubnets
	}

	if dockyardsCluster.Spec.NoDefaultNetworkPlugin {
		controlPlanePatch.Cluster.Network.CNI.Name = ptr.To("none")
	}

	// Authentication configuration
	authenticationConfig := dockyardsCluster.Spec.AuthenticationConfig
	if authenticationConfig != nil {
		content, err := marshalAuthenticationConfig(authenticationConfig)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("could not marshal authentication config: %w", err)
		}

		controlPlanePatch.Machine.Files = append(controlPlanePatch.Machine.Files, talospatchv1.MachineFile{
			Content:     string(content),
			Permissions: 0o444,
			Path:        "/var/manifests/authentication.yaml",
			Op:          "create",
		})

		controlPlanePatch.Cluster.APIServer.ExtraArgs.Add("authentication-config", "/var/manifests/authentication.yaml")
		controlPlanePatch.Cluster.APIServer.ExtraVolumes = append(controlPlanePatch.Cluster.APIServer.ExtraVolumes, talospatchv1.ExtraVolume{
			HostPath:  "/var/manifests/authentication.yaml",
			MountPath: "/var/manifests/authentication.yaml",
			Readonly:  true,
		})
	}

	err = strategicPatches.Add(&controlPlanePatch)
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, nil
	}

	patches := dockyardsCluster.Spec.Advanced.Kubevirt.Talos.AdditionalControlPlaneConfigPatches
	if len(patches) > 0 {
		err = strategicPatches.AddMany(patches)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	talosControlPlane := controlplanev1.TalosControlPlane{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &talosControlPlane, func() error {
		if talosControlPlane.Labels == nil {
			talosControlPlane.Labels = map[string]string{}
		}

		talosControlPlane.Labels[dockyardsv1.LabelClusterName] = dockyardsCluster.Name
		talosControlPlane.Labels[dockyardsv1.LabelOrganizationName] = dockyardsCluster.Labels[dockyardsv1.LabelOrganizationName]

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
				GenerateType:     "controlplane",
				TalosVersion:     "v1.12",
				StrategicPatches: strategicPatches,
			},
		}

		return nil
	})
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, nil
	}

	logger.Info("reconciled talos control plane", "result", operationResult)

	conditions.MarkTrue(dockyardsNodePool, TalosControlPlaneReconciledCondition, ReconciledReason, "")

	return ctrl.Result{}, nil
}

func marshalAuthenticationConfig(authenticationConfig *apiserverv1.AuthenticationConfiguration) ([]byte, error) {
	config := *authenticationConfig
	config.TypeMeta.APIVersion = "apiserver.config.k8s.io/v1"
	config.TypeMeta.Kind = "AuthenticationConfiguration"

	jsonData, err := json.Marshal(&config)
	if err != nil {
		return nil, err
	}

	return sigsyaml.JSONToYAML(jsonData)
}

func (r *DockyardsNodePoolReconciler) reconcileTalosConfigTemplate(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	var strategicPatches StrategicPatches

	err := r.addSharedConfigPatches(dockyardsCluster, &strategicPatches)
	if err != nil {
		return ctrl.Result{}, err
	}

	patches := dockyardsCluster.Spec.Advanced.Kubevirt.Talos.AdditionalWorkerConfigPatches
	if len(patches) > 0 {
		err = strategicPatches.AddMany(patches)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	err = r.addNodePoolNodeLabelsConfigPatch(dockyardsNodePool, &strategicPatches)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.addNodePoolNodeTaintsConfigPatch(dockyardsNodePool, &strategicPatches)
	if err != nil {
		return ctrl.Result{}, err
	}

	talosConfigTemplate := bootstrapv1.TalosConfigTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsNodePool.Name,
			Namespace: dockyardsNodePool.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &talosConfigTemplate, func() error {
		talosConfigTemplate.Spec.Template.Spec.GenerateType = "worker"
		talosConfigTemplate.Spec.Template.Spec.TalosVersion = "v1.12"

		talosConfigTemplate.Spec.Template.Spec.StrategicPatches = strategicPatches

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

	if !dockyardsCluster.Status.APIEndpoint.IsValid() {
		conditions.MarkFalse(dockyardsNodePool, MachineDeploymentReconciledCondition, WaitingForClusterEndpointReason, "")

		return ctrl.Result{}, nil
	}

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
		machineDeployment.Spec.Template.Spec.Version = dockyardsCluster.Spec.Version

		machineDeployment.Spec.Template.Spec.Bootstrap = clusterv1.Bootstrap{
			ConfigRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: bootstrapv1.GroupVersion.Group,
				Kind:     "TalosConfigTemplate",
				Name:     dockyardsNodePool.Name,
			},
		}

		machineDeployment.Spec.Template.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
			APIGroup: providerv1.GroupVersion.Group,
			Kind:     "KubevirtMachineTemplate",
			Name:     dockyardsNodePool.Name,
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

func (patches *StrategicPatches) Add(value yaml.IsZeroer) error {
	if value.IsZero() {
		// Nothing to add :)
		return nil
	}

	raw, err := yaml.Marshal(value)
	if err != nil {
		return fmt.Errorf("could not marshal strategic patch: %w", err)
	}
	*patches = append(*patches, string(raw))
	return nil
}

func (patches *StrategicPatches) AddMany(value []dockyardsv1.Patch) error {
	if len(value) == 0 {
		return nil
	}

	*patches = slices.Grow(*patches, len(value))
	for _, item := range value {
		if len(item.Raw) == 0 {
			continue
		}

		decoded := map[string]any{}
		err := yaml.Unmarshal(item.Raw, &decoded)
		if err != nil {
			return fmt.Errorf("could not decode strategic patch: %w", err)
		}

		result, err := yaml.Marshal(decoded)
		if err != nil {
			return err
		}
		*patches = append(*patches, string(result))
	}
	return nil
}

func parseCommaSeparatedUnique(value string) []string {
	fields := strings.Split(value, ",")
	result := make([]string, 0, len(fields))
	seen := map[string]struct{}{}

	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}

		if _, ok := seen[field]; ok {
			continue
		}

		seen[field] = struct{}{}
		result = append(result, field)
	}

	return result
}
