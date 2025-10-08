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

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"github.com/sudoswedenab/dockyards-kubevirt/controllers"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch

func main() {
	var gatewayName string
	var gatewayNamespace string
	var metricsBindAddress string
	var dockyardsNamespace string
	var dataVolumeStorageClassName string
	var enableMultus bool
	var validNodeIPSubnets []string
	var enableWorkloadIngress bool
	pflag.StringVar(&gatewayName, "gateway-name", "", "gateway name")
	pflag.StringVar(&gatewayNamespace, "gateway-namespace", "", "gateway namespace")
	pflag.StringVar(&metricsBindAddress, "metrics-bind-address", "0", "metrics bind address")
	pflag.StringVar(&dockyardsNamespace, "dockyards-namespace", "dockyards-system", "dockyards namespace")
	pflag.StringVar(&dataVolumeStorageClassName, "data-volume-storage-class-name", "rook-ceph-block", "data volume storage class name")
	pflag.BoolVar(&enableMultus, "enable-multus", false, "enable multus (experimental)")
	pflag.StringSliceVar(&validNodeIPSubnets, "valid-node-ip-subnets", []string{}, "valid node IP subnets")
	pflag.BoolVar(&enableWorkloadIngress, "workload-ingress", true, "enable workload ingress")
	pflag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	slogr := logr.FromSlogHandler(handler)

	ctrl.SetLogger(slogr)

	cfg, err := config.GetConfig()
	if err != nil {
		slogr.Error(err, "error getting config")

		os.Exit(1)
	}

	scheme := runtime.NewScheme()

	_ = clusterv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	opts := manager.Options{
		Metrics: server.Options{
			BindAddress: metricsBindAddress,
		},
		Scheme: scheme,
	}

	mgr, err := ctrl.NewManager(cfg, opts)
	if err != nil {
		slogr.Error(err, "error creating manager")

		os.Exit(1)
	}

	secretClient, err := client.New(mgr.GetConfig(), client.Options{
		HTTPClient: mgr.GetHTTPClient(),
		Cache: &client.CacheOptions{
			Reader: mgr.GetCache(),
		},
	})
	if err != nil {
		slogr.Error(err, "error creating secret client")

		os.Exit(1)
	}

	clusterCache, err := clustercache.SetupWithManager(ctx, mgr, clustercache.Options{
		SecretClient: secretClient,
		Client: clustercache.ClientOptions{
			UserAgent: "kubevirt.dockyards.io",
		},
	}, controller.Options{})
	if err != nil {
		slogr.Error(err, "error creating cluster cache")

		os.Exit(1)
	}

	gatewayParentReference := gatewayapiv1.ParentReference{
		Name:      gatewayapiv1.ObjectName(gatewayName),
		Namespace: ptr.To(gatewayapiv1.Namespace(gatewayNamespace)),
	}

	err = (&controllers.ClusterAPIClusterReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating clusterapi cluster reconciler")

		os.Exit(1)
	}

	err = (&controllers.DockyardsNodePoolReconciler{
		Client:                     mgr.GetClient(),
		DataVolumeStorageClassName: &dataVolumeStorageClassName,
		EnableMultus:               enableMultus,
		ValidNodeIPSubnets:         validNodeIPSubnets,
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards node pool reconciler")

		os.Exit(1)
	}

	err = (&controllers.DockyardsOrganizationReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards organization reconciler")

		os.Exit(1)
	}

	err = (&controllers.DockyardsClusterReconciler{
		Client:                 mgr.GetClient(),
		GatewayParentReference: gatewayParentReference,
		DockyardsNamespace:     dockyardsNamespace,
		EnableWorkloadIngress:  enableWorkloadIngress,
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards cluster reconciler")

		os.Exit(1)
	}

	if enableWorkloadIngress {
		err = (&controllers.DockyardsWorkloadReconciler{
			Client:                 mgr.GetClient(),
			GatewayParentReference: gatewayParentReference,
			ClusterCache:           clusterCache,
		}).SetupWithManager(mgr)
		if err != nil {
			slogr.Error(err, "error creating dockyards deployment reconciler")
		}
	}

	err = (&controllers.DockyardsNodeReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards node reconciler")
	}

	err = (&controllers.DockyardsReleaseReconciler{
		Client:                     mgr.GetClient(),
		DataVolumeStorageClassName: &dataVolumeStorageClassName,
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards release reconciler")
	}

	err = mgr.Start(ctx)
	if err != nil {
		slogr.Error(err, "error starting manager")

		os.Exit(1)
	}
}
