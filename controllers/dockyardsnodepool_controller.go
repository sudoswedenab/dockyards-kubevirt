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
	talospatchv1 "github.com/sudoswedenab/dockyards-kubevirt/internal/talospatch/v1alpha1"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datasources,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=create;get;list;patch;watch
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

type StrategicPatches []string

type DockyardsNodePoolReconciler struct {
	client.Client

	TalosClusterDiscoveryServiceEndpoint string
	DataVolumeStorageClassName           *string
	EnableMultus                         bool
	ValidNodeIPSubnets                   []string
	UseBlockStorage                      bool
	DockyardsConfig                      *dyconfig.ConfigManager
}

const (
	clusterDataVolumeStorageClassNameKey = "dataVolumeStorageClassName"
	clusterTalosInstallerURLKey          = "url"
	clusterTalosInstallerSizeKey         = "size"
	defaultTalosInstallerDataVolumeSize  = "8Gi"
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

func (r *DockyardsNodePoolReconciler) resolveTalosInstallerOverride(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool) (*dockyardsv1.Cluster, *talosInstallerOverride, error) {
	ownerCluster, err := apiutil.GetOwnerCluster(ctx, r.Client, dockyardsNodePool)
	if apierrors.IsNotFound(err) {
		return nil, nil, nil
	}

	if err != nil {
		return nil, nil, err
	}

	if ownerCluster == nil {
		return nil, nil, nil
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
		return nil, nil, err
	}

	customTalosInstallerURL, found, err := unstructured.NestedString(unstructuredDockyardsCluster.Object, "spec", "advanced", "kubevirt", "talos", "installImage", clusterTalosInstallerURLKey)
	if err != nil {
		return nil, nil, err
	}

	customTalosInstallerURL = strings.TrimSpace(customTalosInstallerURL)
	if !found || customTalosInstallerURL == "" {
		return ownerCluster, nil, nil
	}

	talosInstallerSizeRaw, _, err := unstructured.NestedString(unstructuredDockyardsCluster.Object, "spec", "advanced", "kubevirt", "talos", "installImage", clusterTalosInstallerSizeKey)
	if err != nil {
		return nil, nil, err
	}

	talosInstallerSizeRaw = strings.TrimSpace(talosInstallerSizeRaw)
	if talosInstallerSizeRaw == "" {
		talosInstallerSizeRaw = defaultTalosInstallerDataVolumeSize
	}

	talosInstallerSize, err := resource.ParseQuantity(talosInstallerSizeRaw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid talos installer size %q: %w", talosInstallerSizeRaw, err)
	}

	return ownerCluster, &talosInstallerOverride{
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

	if ownerCluster == nil {
		return r.DataVolumeStorageClassName, nil
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

	clusterStorageClassName, found, err := unstructured.NestedString(unstructuredDockyardsCluster.Object, "spec", "advanced", "kubevirt", clusterDataVolumeStorageClassNameKey)
	if err != nil {
		return nil, err
	}

	clusterStorageClassName = strings.TrimSpace(clusterStorageClassName)
	if found && clusterStorageClassName != "" {
		return ptr.To(clusterStorageClassName), nil
	}

	return r.DataVolumeStorageClassName, nil
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

func (r *DockyardsNodePoolReconciler) addNodePoolNodeLabelsConfigPatch(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, strategicPatches *StrategicPatches) error {
	unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": dockyardsv1.GroupVersion.String(),
			"kind":       dockyardsv1.NodePoolKind,
			"metadata": map[string]any{
				"name":      dockyardsNodePool.Name,
				"namespace": dockyardsNodePool.Namespace,
			},
		},
	}

	err := r.Get(ctx, client.ObjectKeyFromObject(&unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend), &unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend)
	if err != nil {
		return fmt.Errorf("could not get unstructured nodepool object: %w", err)
	}

	value := unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend.Object
	labels, found, err := unstructured.NestedStringMap(value, "spec", "nodeLabels")
	if err != nil {
		return fmt.Errorf("could not read spec.nodeLabels: %w", err)
	}
	if !found || len(labels) == 0 {
		return nil
	}

	patch := r.labelConfigPatch(labels)
	err = strategicPatches.Add(ptr.To(patch))
	if err != nil {
		return fmt.Errorf("could not add node labels strategic patch: %w", err)
	}

	return nil
}

func (r *DockyardsNodePoolReconciler) addNodePoolNodeTaintsConfigPatch(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, strategicPatches *StrategicPatches) error {
	unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": dockyardsv1.GroupVersion.String(),
			"kind":       dockyardsv1.NodePoolKind,
			"metadata": map[string]any{
				"name":      dockyardsNodePool.Name,
				"namespace": dockyardsNodePool.Namespace,
			},
		},
	}

	err := r.Get(ctx, client.ObjectKeyFromObject(&unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend), &unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend)
	if err != nil {
		return fmt.Errorf("could not get unstructured nodepool object: %w", err)
	}

	value := unstructuredDockyardsNodePoolFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackend.Object
	taints, found, err := unstructured.NestedStringMap(value, "spec", "nodeTaints")
	if err != nil {
		return fmt.Errorf("could not read spec.nodeTaints: %w", err)
	}
	if !found || len(taints) == 0 {
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
	err = strategicPatches.Add(ptr.To(patch))
	if err != nil {
		return fmt.Errorf("could not add node taints strategic patch: %w", err)
	}

	return nil
}

func (r *DockyardsNodePoolReconciler) addSharedConfigPatches(
	dockyardsCluster *dockyardsv1.Cluster,
	unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI unstructured.Unstructured,
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

	value := unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI.Object
	patches, found, err := unstructured.NestedSlice(value, "spec", "advanced", "kubevirt", "talos", "additionalSharedConfigPatches")
	if found && err == nil {
		err = strategicPatches.AddManyUnstructured(patches)
		if err != nil {
			return err
		}
	}

	return nil
}

func (r *DockyardsNodePoolReconciler) reconcileTalosControlPlane(ctx context.Context, dockyardsNodePool *dockyardsv1.NodePool, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "dockyards.io/v1alpha3",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      dockyardsCluster.Name,
				"namespace": dockyardsCluster.Namespace,
			},
		},
	}

	err := r.Get(ctx, client.ObjectKeyFromObject(&unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI), &unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("could not get unstructured cluster object: %w", err)
	}

	if !dockyardsCluster.Status.APIEndpoint.IsValid() {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, WaitingForClusterEndpointReason, "")

		return ctrl.Result{}, nil
	}

	var strategicPatches StrategicPatches

	err = r.addSharedConfigPatches(dockyardsCluster, unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI, &strategicPatches)
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, nil
	}

	err = r.addNodePoolNodeLabelsConfigPatch(ctx, dockyardsNodePool, &strategicPatches)
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, nil
	}

	err = r.addNodePoolNodeTaintsConfigPatch(ctx, dockyardsNodePool, &strategicPatches)
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
	obj := unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI.Object
	authenticationConfig, found, err := unstructured.NestedMap(obj, "spec", "authenticationConfig")
	if err == nil && found {
		content, err := yaml.Marshal(authenticationConfig)
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

	value := unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI.Object
	patches, found, err := unstructured.NestedSlice(value, "spec", "advanced", "kubevirt", "talos", "additionalControlPlaneConfigPatches")
	if found && err == nil {
		err = strategicPatches.AddManyUnstructured(patches)
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

	unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "dockyards.io/v1alpha3",
			"kind":       "Cluster",
			"metadata": map[string]any{
				"name":      dockyardsCluster.Name,
				"namespace": dockyardsCluster.Namespace,
			},
		},
	}

	err := r.Get(ctx, client.ObjectKeyFromObject(&unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI), &unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("could not get unstructured cluster object: %w", err)
	}

	var strategicPatches StrategicPatches

	err = r.addSharedConfigPatches(dockyardsCluster, unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI, &strategicPatches)
	if err != nil {
		return ctrl.Result{}, err
	}

	value := unstructuredDockyardsClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI.Object
	patches, found, err := unstructured.NestedSlice(value, "spec", "advanced", "kubevirt", "talos", "additionalWorkerConfigPatches")
	if found && err == nil {
		err = strategicPatches.AddManyUnstructured(patches)
		if err != nil {
			return ctrl.Result{}, err
		}
	}

	err = r.addNodePoolNodeLabelsConfigPatch(ctx, dockyardsNodePool, &strategicPatches)
	if err != nil {
		return ctrl.Result{}, err
	}

	err = r.addNodePoolNodeTaintsConfigPatch(ctx, dockyardsNodePool, &strategicPatches)
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

func (patches *StrategicPatches) AddManyUnstructured(value []any) error {
	if len(value) == 0 {
		// Nothing to add :)
		return nil
	}

	*patches = slices.Grow(*patches, len(value))
	for _, item := range value {
		result, err := yaml.Marshal(item)
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
