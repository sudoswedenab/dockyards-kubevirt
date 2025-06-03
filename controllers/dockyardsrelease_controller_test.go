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

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"github.com/sudoswedenab/dockyards-kubevirt/test/mockcrds"
	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func TestDockyardsReleaseReconciler_ReconcileDataVolume(t *testing.T) {
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

	dataVolumeStorageClassName := "test-block"

	reconciler := DockyardsReleaseReconciler{
		Client:                     mgr.GetClient(),
		DataVolumeStorageClassName: &dataVolumeStorageClassName,
	}

	t.Run("test release", func(t *testing.T) {
		release := dockyardsv1.Release{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: "test-",
				Namespace:    namespace.Name,
			},
			Spec: dockyardsv1.ReleaseSpec{
				Type: dockyardsv1.ReleaseTypeTalosInstaller,
				Ranges: []string{
					"v1.2.x",
				},
			},
		}

		err := c.Create(ctx, &release)
		if err != nil {
			t.Fatal(err)
		}

		release.Status.LatestVersion = "v1.2.3"
		release.Status.LatestURL = ptr.To("http://localhost:1234/testing/openstack-amd64.raw.xz")

		_, err = reconciler.reconcileDataVolume(ctx, &release)
		if err != nil {
			t.Fatal(err)
		}

		objectKey := client.ObjectKey{
			Name:      release.Name + "-" + release.Status.LatestVersion,
			Namespace: release.Namespace,
		}

		var actualDataVolume cdiv1.DataVolume
		err = c.Get(ctx, objectKey, &actualDataVolume)
		if err != nil {
			t.Fatal(err)
		}

		expectedDataVolume := cdiv1.DataVolume{
			ObjectMeta: metav1.ObjectMeta{
				Annotations: map[string]string{
					"cdi.kubevirt.io/storage.bind.immediate.requested": "",
				},
				Labels: map[string]string{
					dockyardsv1.LabelReleaseName: release.Name,
				},
				Name:      release.Name + "-" + release.Status.LatestVersion,
				Namespace: release.Namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: dockyardsv1.GroupVersion.String(),
						Kind:       dockyardsv1.ReleaseKind,
						Name:       release.Name,
						UID:        release.UID,
					},
				},
				//
				CreationTimestamp: actualDataVolume.CreationTimestamp,
				Generation:        actualDataVolume.Generation,
				ManagedFields:     actualDataVolume.ManagedFields,
				ResourceVersion:   actualDataVolume.ResourceVersion,
				UID:               actualDataVolume.UID,
			},
			Spec: cdiv1.DataVolumeSpec{
				Source: &cdiv1.DataVolumeSource{
					HTTP: &cdiv1.DataVolumeSourceHTTP{
						URL: *release.Status.LatestURL,
					},
				},
				Storage: &cdiv1.StorageSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{
						corev1.ReadWriteMany,
					},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("2Gi"),
						},
					},
					StorageClassName: &dataVolumeStorageClassName,
					VolumeMode:       ptr.To(corev1.PersistentVolumeBlock),
				},
			},
		}

		if !cmp.Equal(actualDataVolume, expectedDataVolume) {
			t.Errorf("diff: %s", cmp.Diff(expectedDataVolume, actualDataVolume))
		}

		var actualDataSource cdiv1.DataSource
		err = c.Get(ctx, client.ObjectKeyFromObject(&release), &actualDataSource)
		if err != nil {
			t.Fatal(err)
		}

		expectedDataSource := cdiv1.DataSource{
			ObjectMeta: metav1.ObjectMeta{
				Labels: map[string]string{
					dockyardsv1.LabelReleaseName: release.Name,
				},
				Name:      release.Name,
				Namespace: release.Namespace,
				//
				CreationTimestamp: actualDataSource.CreationTimestamp,
				Generation:        actualDataSource.Generation,
				ManagedFields:     actualDataSource.ManagedFields,
				ResourceVersion:   actualDataSource.ResourceVersion,
				UID:               actualDataSource.UID,
			},
			Spec: cdiv1.DataSourceSpec{
				Source: cdiv1.DataSourceSource{
					PVC: &cdiv1.DataVolumeSourcePVC{
						Name:      actualDataVolume.Name,
						Namespace: actualDataVolume.Namespace,
					},
				},
			},
		}

		if !cmp.Equal(actualDataSource, expectedDataSource) {
			t.Errorf("diff: %s", cmp.Diff(expectedDataSource, actualDataSource))
		}
	})
}
