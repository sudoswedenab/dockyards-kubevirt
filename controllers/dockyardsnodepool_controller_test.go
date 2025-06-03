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
	"strconv"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"github.com/sudoswedenab/dockyards-kubevirt/test/mockcrds"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func TestDockyardsNodePoolReconciler_ReconcileMachineTemplate(t *testing.T) {
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

	_ = cdiv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	systemNamespace := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "system-",
		},
	}

	err = c.Create(ctx, &systemNamespace)
	if err != nil {
		t.Fatal(err)
	}

	release := dockyardsv1.Release{
		ObjectMeta: metav1.ObjectMeta{
			Annotations: map[string]string{
				dockyardsv1.AnnotationDefaultRelease: "true",
			},
			GenerateName: "talos-",
			Namespace:    systemNamespace.Name,
		},
		Spec: dockyardsv1.ReleaseSpec{
			Type: dockyardsv1.ReleaseTypeTalosInstaller,
		},
	}

	err = c.Create(ctx, &release)
	if err != nil {
		t.Fatal(err)
	}

	dataVolumeStorageClassName := "test-block"

	dataSource := cdiv1.DataSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.Name,
			Namespace: release.Namespace,
		},
		Spec: cdiv1.DataSourceSpec{
			Source: cdiv1.DataSourceSource{
				PVC: &cdiv1.DataVolumeSourcePVC{
					Name:      release.Name,
					Namespace: release.Namespace,
				},
			},
		},
	}

	err = c.Create(ctx, &dataSource)
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

	reconciler := DockyardsNodePoolReconciler{
		Client:                     mgr.GetClient(),
		DataVolumeStorageClassName: &dataVolumeStorageClassName,
	}

	t.Run("test resources", func(t *testing.T) {
		nodePool := dockyardsv1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
			Spec: dockyardsv1.NodePoolSpec{
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:     resource.MustParse("2"),
					corev1.ResourceMemory:  resource.MustParse("2Gi"),
					corev1.ResourceStorage: resource.MustParse("8G"),
				},
			},
		}

		err := c.Create(ctx, &nodePool)
		if err != nil {
			t.Fatal(err)
		}

		_, err = reconciler.reconcileMachineTemplate(ctx, &nodePool)
		if err != nil {
			t.Fatal(err)
		}

		var actual providerv1.KubevirtMachineTemplate
		err = c.Get(ctx, client.ObjectKeyFromObject(&nodePool), &actual)
		if err != nil {
			t.Fatal(err)
		}

		expected := providerv1.KubevirtMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodePool.Name,
				Namespace: nodePool.Namespace,
				UID:       actual.UID,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: dockyardsv1.GroupVersion.String(),
						Kind:       dockyardsv1.NodePoolKind,
						Name:       nodePool.Name,
						UID:        nodePool.UID,
					},
				},
				//
				CreationTimestamp: actual.CreationTimestamp,
				Generation:        actual.Generation,
				ManagedFields:     actual.ManagedFields,
				ResourceVersion:   actual.ResourceVersion,
			},
			Spec: providerv1.KubevirtMachineTemplateSpec{
				Template: providerv1.KubevirtMachineTemplateResource{
					Spec: providerv1.KubevirtMachineSpec{
						VirtualMachineTemplate: providerv1.VirtualMachineTemplateSpec{
							Spec: kubevirtv1.VirtualMachineSpec{
								RunStrategy: ptr.To(kubevirtv1.RunStrategyAlways),
								Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
									Spec: kubevirtv1.VirtualMachineInstanceSpec{
										Domain: kubevirtv1.DomainSpec{
											CPU: &kubevirtv1.CPU{
												Cores: 2,
											},
											Devices: kubevirtv1.Devices{
												Disks: []kubevirtv1.Disk{
													{
														DiskDevice: kubevirtv1.DiskDevice{
															Disk: &kubevirtv1.DiskTarget{
																Bus: kubevirtv1.DiskBusVirtio,
															},
														},
														Name: "boot",
													},
												},
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
												Guest: ptr.To(resource.MustParse("2Gi")),
											},
										},
										EvictionStrategy: ptr.To(kubevirtv1.EvictionStrategyLiveMigrate),
										Volumes: []kubevirtv1.Volume{
											{
												Name: "boot",
												VolumeSource: kubevirtv1.VolumeSource{
													DataVolume: &kubevirtv1.DataVolumeSource{
														Name: "boot",
													},
												},
											},
										},
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
								DataVolumeTemplates: []kubevirtv1.DataVolumeTemplateSpec{
									{
										ObjectMeta: metav1.ObjectMeta{
											Name: "boot",
										},
										Spec: cdiv1.DataVolumeSpec{
											SourceRef: &cdiv1.DataVolumeSourceRef{
												Kind:      "DataSource",
												Name:      dataSource.Name,
												Namespace: &dataSource.Namespace,
											},
											PVC: &corev1.PersistentVolumeClaimSpec{
												AccessModes: []corev1.PersistentVolumeAccessMode{
													corev1.ReadWriteMany,
												},
												Resources: corev1.VolumeResourceRequirements{
													Requests: corev1.ResourceList{
														corev1.ResourceStorage: resource.MustParse("8G"),
													},
												},
												StorageClassName: &dataVolumeStorageClassName,
												VolumeMode:       ptr.To(corev1.PersistentVolumeBlock),
											},
										},
									},
								},
							},
						},
						BootstrapCheckSpec: providerv1.VirtualMachineBootstrapCheckSpec{
							CheckStrategy: "none",
						},
					},
				},
			},
		}

		if !cmp.Equal(actual, expected) {
			t.Errorf("diff: %s", cmp.Diff(expected, actual))
		}
	})

	t.Run("test storage resources", func(t *testing.T) {
		nodePool := dockyardsv1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
			Spec: dockyardsv1.NodePoolSpec{
				Resources: corev1.ResourceList{
					corev1.ResourceCPU:     resource.MustParse("2"),
					corev1.ResourceMemory:  resource.MustParse("2Gi"),
					corev1.ResourceStorage: resource.MustParse("8G"),
				},
				StorageResources: []dockyardsv1.NodePoolStorageResource{
					{
						Name:     "test",
						Quantity: resource.MustParse("10Gi"),
					},
				},
			},
		}

		err := c.Create(ctx, &nodePool)
		if err != nil {
			t.Fatal(err)
		}

		_, err = reconciler.reconcileMachineTemplate(ctx, &nodePool)
		if err != nil {
			t.Fatal(err)
		}

		var actual providerv1.KubevirtMachineTemplate
		err = c.Get(ctx, client.ObjectKeyFromObject(&nodePool), &actual)
		if err != nil {
			t.Fatal(err)
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
			{
				DiskDevice: kubevirtv1.DiskDevice{
					Disk: &kubevirtv1.DiskTarget{
						Bus: kubevirtv1.DiskBusVirtio,
					},
				},
				Name: "test",
			},
		}

		volumes := []kubevirtv1.Volume{
			{
				Name: "boot",
				VolumeSource: kubevirtv1.VolumeSource{
					DataVolume: &kubevirtv1.DataVolumeSource{
						Name: "boot",
					},
				},
			},
			{
				Name: "test",
				VolumeSource: kubevirtv1.VolumeSource{
					DataVolume: &kubevirtv1.DataVolumeSource{
						Name: "test",
					},
				},
			},
		}

		dataVolumeTemplates := []kubevirtv1.DataVolumeTemplateSpec{
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "boot",
				},
				Spec: cdiv1.DataVolumeSpec{
					SourceRef: &cdiv1.DataVolumeSourceRef{
						Kind:      "DataSource",
						Name:      dataSource.Name,
						Namespace: &dataSource.Namespace,
					},
					PVC: &corev1.PersistentVolumeClaimSpec{
						AccessModes: []corev1.PersistentVolumeAccessMode{
							corev1.ReadWriteMany,
						},
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: resource.MustParse("8G"),
							},
						},
						StorageClassName: &dataVolumeStorageClassName,
						VolumeMode:       ptr.To(corev1.PersistentVolumeBlock),
					},
				},
			},
			{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test",
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
								corev1.ResourceStorage: resource.MustParse("10Gi"),
							},
						},
						StorageClassName: &dataVolumeStorageClassName,
						VolumeMode:       ptr.To(corev1.PersistentVolumeBlock),
					},
				},
			},
		}

		expected := providerv1.KubevirtMachineTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:      nodePool.Name,
				Namespace: nodePool.Namespace,
				UID:       actual.UID,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: dockyardsv1.GroupVersion.String(),
						Kind:       dockyardsv1.NodePoolKind,
						Name:       nodePool.Name,
						UID:        nodePool.UID,
					},
				},
				//
				CreationTimestamp: actual.CreationTimestamp,
				Generation:        actual.Generation,
				ManagedFields:     actual.ManagedFields,
				ResourceVersion:   actual.ResourceVersion,
			},
			Spec: providerv1.KubevirtMachineTemplateSpec{
				Template: providerv1.KubevirtMachineTemplateResource{
					Spec: providerv1.KubevirtMachineSpec{
						VirtualMachineTemplate: providerv1.VirtualMachineTemplateSpec{
							Spec: kubevirtv1.VirtualMachineSpec{
								RunStrategy: ptr.To(kubevirtv1.RunStrategyAlways),
								Template: &kubevirtv1.VirtualMachineInstanceTemplateSpec{
									Spec: kubevirtv1.VirtualMachineInstanceSpec{
										Domain: kubevirtv1.DomainSpec{
											CPU: &kubevirtv1.CPU{
												Cores: 2,
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
												Guest: ptr.To(resource.MustParse("2Gi")),
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
								DataVolumeTemplates: dataVolumeTemplates,
							},
						},
						BootstrapCheckSpec: providerv1.VirtualMachineBootstrapCheckSpec{
							CheckStrategy: "none",
						},
					},
				},
			},
		}

		if !cmp.Equal(actual, expected) {
			t.Errorf("diff: %s", cmp.Diff(expected, actual))
		}
	})
}

func TestDockyardsNodePoolReconciler_ReconcileTalosControlPlane(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("no kubebuilder assets configured")
	}

	env := envtest.Environment{
		CRDs: mockcrds.CRDs,
	}

	textHandler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError})
	slogr := logr.FromSlogHandler(textHandler)

	ctrl.SetLogger(slogr)

	ctx := t.Context()

	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		err := env.Stop()
		if err != nil {
			panic(err)
		}
	})

	scheme := runtime.NewScheme()

	_ = clusterv1.AddToScheme(scheme)
	_ = controlplanev1.AddToScheme(scheme)
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

	reconciler := DockyardsNodePoolReconciler{
		Client: mgr.GetClient(),
	}

	t.Run("test network plugin", func(t *testing.T) {
		owner := dockyardsv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
			Spec: dockyardsv1.ClusterSpec{
				NoDefaultNetworkPlugin: true,
			},
		}

		err := c.Create(ctx, &owner)
		if err != nil {
			t.Fatal(err)
		}

		patch := client.MergeFrom(owner.DeepCopy())

		owner.Status.APIEndpoint = dockyardsv1.ClusterAPIEndpoint{
			Host: "localhost",
			Port: 6443,
		}

		err = c.Status().Patch(ctx, &owner, patch)
		if err != nil {
			t.Fatal(err)
		}

		cluster := clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      owner.Name,
				Namespace: owner.Namespace,
			},
		}

		err = c.Create(ctx, &cluster)
		if err != nil {
			t.Fatal(err)
		}

		nodePool := dockyardsv1.NodePool{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: owner.Name + "-test-",
				Namespace:    owner.Namespace,
			},
		}

		err = c.Create(ctx, &nodePool)
		if err != nil {
			t.Fatal(err)
		}

		_, err = reconciler.reconcileTalosControlPlane(ctx, &nodePool, &owner)
		if err != nil {
			t.Fatal(err)
		}

		var actual controlplanev1.TalosControlPlane
		err = c.Get(ctx, client.ObjectKeyFromObject(&nodePool), &actual)
		if err != nil {
			t.Fatal(err)
		}

		expected := controlplanev1.TalosControlPlane{
			ObjectMeta: actual.ObjectMeta,
			Spec: controlplanev1.TalosControlPlaneSpec{
				ControlPlaneConfig: controlplanev1.ControlPlaneConfig{
					ControlPlaneConfig: bootstrapv1.TalosConfigSpec{
						GenerateType: "controlplane",
						ConfigPatches: []bootstrapv1.ConfigPatches{
							{
								Op:   "replace",
								Path: "/cluster/network/podSubnets",
								Value: apiextensionsv1.JSON{
									Raw: []byte("[" + strconv.Quote("10.128.0.0/16") + "]"),
								},
							},
							{
								Op:   "replace",
								Path: "/cluster/network/serviceSubnets",
								Value: apiextensionsv1.JSON{
									Raw: []byte("[" + strconv.Quote("10.112.0.0/12") + "]"),
								},
							},
							{
								Op:   "replace",
								Path: "/cluster/apiServer/certSANs",
								Value: apiextensionsv1.JSON{
									Raw: []byte(strconv.Quote(owner.Status.APIEndpoint.Host)),
								},
							},
							{
								Op:   "replace",
								Path: "/cluster/network/cni/name",
								Value: apiextensionsv1.JSON{
									Raw: []byte(strconv.Quote("none")),
								},
							},
						},
						TalosVersion: "v1.7",
					},
				},
				InfrastructureTemplate: corev1.ObjectReference{
					APIVersion: providerv1.GroupVersion.String(),
					Kind:       "KubevirtMachineTemplate",
					Name:       nodePool.Name,
					Namespace:  nodePool.Namespace,
				},
			},
		}

		if !cmp.Equal(actual, expected) {
			t.Errorf("diff: %s", cmp.Diff(expected, actual))
		}
	})
}
