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
	"fmt"
	"sort"
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

	// Allow users to ignore interfaces which should not be managed by Talos.
	// Config key is expected to be a comma-separated list, e.g. "eth0,eth1".
	if value, found := r.DockyardsConfig.GetValueForKey(KeyIgnoreInterfaces); found {
		interfaces := parseCommaSeparatedUnique(value)
		if len(interfaces) > 0 {
			sort.Strings(interfaces)
			patch.Machine.Network.Interfaces = make([]talospatchv1.MachineInterface, 0, len(interfaces))
			for _, name := range interfaces {
				patch.Machine.Network.Interfaces = append(patch.Machine.Network.Interfaces, talospatchv1.MachineInterface{
					Interface: name,
					Ignore:    true,
				})
			}
		}
	}

	if r.TalosClusterDiscoveryServiceEndpoint == "0" {
		patch.Cluster.Discovery.Registries.Service.Disabled = ptr.To(true)
	} else {
		patch.Cluster.Discovery.Registries.Service.Endpoint = r.TalosClusterDiscoveryServiceEndpoint
	}

	return patch
}

func (r *DockyardsNodePoolReconciler) timeSyncConfigPatch(dockyardsCluster *dockyardsv1.Cluster) talospatchv1.TimeSyncConfig {
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

func (r *DockyardsNodePoolReconciler) dhcpv4ConfigPatches() ([]talospatchv1.DHCPv4Config, error) {
	// Configure DHCPv4 using the Talos DHCPv4Config document (Talos v1.12+).
	//
	// The Dockyards config map value is expected to be a comma-separated list of interface names.
	// Example:
	// apiVersion: v1alpha1
	// kind: DHCPv4Config
	// name: eth1
	value, found := r.DockyardsConfig.GetValueForKey(KeyDHCPv4Ifaces)
	if !found {
		return nil, nil
	}

	names := parseCommaSeparatedUnique(value)
	if len(names) == 0 {
		return nil, nil
	}

	sort.Strings(names)
	patches := make([]talospatchv1.DHCPv4Config, 0, len(names))
	for _, name := range names {
		patches = append(patches, talospatchv1.DHCPv4Config{
			Meta: talospatchv1.Meta{
				APIVersion: talospatchv1.DHCPv4ConfigAPIVersion,
				Kind:       talospatchv1.DHCPv4ConfigKind,
			},
			Name: name,
		})
	}

	return patches, nil
}

func (r *DockyardsNodePoolReconciler) staticRoutesConfigPatches() ([]talospatchv1.LinkConfig, error) {
	// Configure static routes using Talos LinkConfig routes (Talos v1.12+).
	//
	// The Dockyards config map value is expected to be a YAML/JSON map of interface name -> list of RouteConfig objects.
	// Example:
	// eth0:
	//   - destination: 10.0.0.0/8
	//     gateway: 10.0.0.1
	//     metric: 100
	value, found := r.DockyardsConfig.GetValueForKey(KeyStaticRoutes)
	if !found {
		return nil, nil
	}

	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	var routesByInterface map[string][]talospatchv1.RouteConfig
	err := yaml.Unmarshal([]byte(value), &routesByInterface)
	if err != nil {
		return nil, fmt.Errorf("could not parse %s: %w", KeyStaticRoutes, err)
	}

	if len(routesByInterface) == 0 {
		return nil, nil
	}

	trimmed := make(map[string][]talospatchv1.RouteConfig, len(routesByInterface))
	names := make([]string, 0, len(routesByInterface))
	for name, routes := range routesByInterface {
		name = strings.TrimSpace(name)
		if name == "" {
			return nil, fmt.Errorf("%s: interface name must not be empty", KeyStaticRoutes)
		}
		if len(routes) == 0 {
			continue
		}
		trimmed[name] = routes
		names = append(names, name)
	}

	if len(names) == 0 {
		return nil, nil
	}

	sort.Strings(names)
	patches := make([]talospatchv1.LinkConfig, 0, len(names))
	for _, name := range names {
		patches = append(patches, talospatchv1.LinkConfig{
			Meta: talospatchv1.Meta{
				APIVersion: talospatchv1.LinkConfigAPIVersion,
				Kind:       talospatchv1.LinkConfigKind,
			},
			Name:   name,
			Routes: trimmed[name],
		})
	}

	return patches, nil
}

func (r *DockyardsNodePoolReconciler) addSharedConfigPatches(
	ctx context.Context, // FIXME: Remove context parameter when we no longer need to Get unstructured cluster
	dockyardsCluster *dockyardsv1.Cluster,
	strategicPatches *StrategicPatches,
) error {
	err := strategicPatches.Add(ptr.To(r.talosConfigPatch(dockyardsCluster)))
	if err != nil {
		return fmt.Errorf("could not add talos config patches: %w", err)
	}

	err = strategicPatches.Add(ptr.To(r.timeSyncConfigPatch(dockyardsCluster)))
	if err != nil {
		return fmt.Errorf("could not add time sync config patches: %w", err)
	}

	dhcpPatches, err := r.dhcpv4ConfigPatches()
	if err != nil {
		return err
	}
	for i := range dhcpPatches {
		if err := strategicPatches.Add(&dhcpPatches[i]); err != nil {
			return fmt.Errorf("could not add dhcpv4 config patch: %w", err)
		}
	}

	staticRoutesPatches, err := r.staticRoutesConfigPatches()
	if err != nil {
		return err
	}
	for i := range staticRoutesPatches {
		if err := strategicPatches.Add(&staticRoutesPatches[i]); err != nil {
			return fmt.Errorf("could not add static routes patch: %w", err)
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

	err := r.addSharedConfigPatches(ctx, dockyardsCluster, &strategicPatches)
	if err != nil {
		conditions.MarkFalse(dockyardsNodePool, TalosControlPlaneReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, nil
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

	unstructuredClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI := unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "dockyards.io/v1alpha3",
			"kind":       "Cluster",
			"metadata": map[string]interface{}{
				"name":      dockyardsCluster.Name,
				"namespace": dockyardsCluster.Namespace,
			},
		},
	}

	err = r.Get(ctx, client.ObjectKeyFromObject(&unstructuredClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI), &unstructuredClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("could not get unstructured cluster object: %w", err)
	}

	// Authentication configuration
	obj := unstructuredClusterFIXMERemoveThisWhenTalosHasUpdatedClusterAPIAndSoWeCanUpdateBackendAPI.Object
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

	var strategicPatches StrategicPatches

	err := r.addSharedConfigPatches(ctx, dockyardsCluster, &strategicPatches)
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
