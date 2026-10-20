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
	"net/http"
	"os"
	"os/signal"

	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	dyconfig "github.com/sudoswedenab/dockyards-backend/api/config"
	"github.com/sudoswedenab/dockyards-kubevirt/controllers"
	"github.com/sudoswedenab/dockyards-kubevirt/internal/api/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"slices"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// +kubebuilder:rbac:groups=core,resources=secrets;configmaps,verbs=get;list;watch

func main() {
	var gatewayName string
	var gatewayNamespace string
	var metricsBindAddress string
	var dockyardsSystemNamespace string
	var configMap string
	var dataVolumeStorageClassName string
	var enableMultus bool
	var validNodeIPSubnets []string
	var enableWorkloadIngress bool
	var useBlockStorage bool
	var talosClusterDiscoveryServiceEndpoint string
	var networkInterfaceMultiqueue bool
	var enableReconcilers bool
	pflag.StringVar(&gatewayName, "gateway-name", "", "gateway name")
	pflag.StringVar(&gatewayNamespace, "gateway-namespace", "", "gateway namespace")
	pflag.StringVar(&metricsBindAddress, "metrics-bind-address", "0", "metrics bind address")
	pflag.StringVar(&configMap, "config-map", "dockyards-system", "ConfigMap name")
	pflag.StringVar(&dockyardsSystemNamespace, "dockyards-namespace", "dockyards-system", "dockyards system namespace")
	pflag.StringVar(&dataVolumeStorageClassName, "data-volume-storage-class-name", "rook-ceph-block", "data volume storage class name")
	pflag.BoolVar(&enableMultus, "enable-multus", false, "enable multus (experimental)")
	pflag.BoolVar(&useBlockStorage, "use-block-storage", true, "use block storage")
	pflag.StringSliceVar(&validNodeIPSubnets, "valid-node-ip-subnets", []string{}, "valid node IP subnets")
	pflag.BoolVar(&enableWorkloadIngress, "workload-ingress", true, "enable workload ingress")
	pflag.StringVar(&talosClusterDiscoveryServiceEndpoint, "talos-discovery-service-endpoint", "", "use talos cluster discovery service other than the default one provided by talos, set this to '0' to disable use of discovery service")
	pflag.BoolVar(&networkInterfaceMultiqueue, "network-interface-multiqueue-default", true, "enable network interface multiqueue on all vm interfaces")
	pflag.BoolVar(&enableReconcilers, "enable-reconcilers", true, "enable reconcilers")
	pflag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	handler := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger := slog.New(handler)

	slogr := logr.FromSlogHandler(logger.Handler())
	ctrl.SetLogger(slogr)

	cfg, err := config.GetConfig()
	if err != nil {
		slogr.Error(err, "error getting config")

		os.Exit(1)
	}

	scheme := runtime.NewScheme()

	_ = clusterv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = authorizationv1.AddToScheme(scheme)

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

	configManagerOptions := []dyconfig.ConfigManagerOption{
		dyconfig.WithLogger(logger),
	}
	dockyardsConfig, err := dyconfig.NewConfigManager(mgr, client.ObjectKey{Namespace: dockyardsSystemNamespace, Name: configMap}, configManagerOptions...)
	if err != nil {
		slogr.Error(err, "error getting dockyards config")

		os.Exit(1)
	}

	if enableReconcilers {
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

		err = (&controllers.KubevirtVirtualMachineReconciler{
			Client: mgr.GetClient(),
		}).SetupWithManager(mgr)
		if err != nil {
			slogr.Error(err, "error creating kubevirt virtualmachine reconciler")

			os.Exit(1)
		}

		err = (&controllers.DockyardsNodePoolReconciler{
			Client:                               mgr.GetClient(),
			TalosClusterDiscoveryServiceEndpoint: talosClusterDiscoveryServiceEndpoint,
			DataVolumeStorageClassName:           &dataVolumeStorageClassName,
			EnableMultus:                         enableMultus,
			UseBlockStorage:                      useBlockStorage,
			ValidNodeIPSubnets:                   validNodeIPSubnets,
			DockyardsConfig:                      dockyardsConfig,
			NetworkInterfaceMultiQueue:           networkInterfaceMultiqueue,
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
			Client:                   mgr.GetClient(),
			GatewayParentReference:   gatewayParentReference,
			DockyardsSystemNamespace: dockyardsSystemNamespace,
			DockyardsConfig:          dockyardsConfig,
			EnableWorkloadIngress:    enableWorkloadIngress,
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

				os.Exit(1)
			}
		}

		err = (&controllers.DockyardsNodeReconciler{
			Client: mgr.GetClient(),
		}).SetupWithManager(mgr)
		if err != nil {
			slogr.Error(err, "error creating dockyards node reconciler")

			os.Exit(1)
		}

		err = (&controllers.DockyardsMachineIPReconciler{
			Client: mgr.GetClient(),
		}).SetupWithManager(mgr)
		if err != nil {
			slogr.Error(err, "error creating dockyards machine IP reconciler")

			os.Exit(1)
		}

		err = (&controllers.DockyardsReleaseReconciler{
			Client:                     mgr.GetClient(),
			DataVolumeStorageClassName: &dataVolumeStorageClassName,
			UseBlockStorage:            useBlockStorage,
		}).SetupWithManager(mgr)
		if err != nil {
			slogr.Error(err, "error creating dockyards release reconciler")

			os.Exit(1)
		}

	}

	publicMux := http.NewServeMux()

	api, err := v1.NewAPI(mgr, dockyardsConfig, dockyardsSystemNamespace)
	if err != nil {
		slogr.Error(err, "error creating api")

		os.Exit(1)
	}

	api.RegisterRoutes(publicMux)

	public := cors(dockyardsConfig, publicMux)

	go func() {
		addr := ":9002"
		logger.Info("serving http", "addr", addr)
		err := http.ListenAndServe(addr, public)
		if err != nil {
			logger.Error("error running server", "err", err)

			os.Exit(1)
		}
	}()

	err = mgr.Start(ctx)
	if err != nil {
		slogr.Error(err, "error starting manager")

		os.Exit(1)
	}
}

func cors(config *dyconfig.ConfigManager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" { // Request was not made by browser, so CORS is not relevant.
			next.ServeHTTP(w, r)

			return
		}

		allowedOrigins := config.GetStringSliceOrDefault(dyconfig.KeyAllowedOrigins, nil)
		if !slices.Contains(allowedOrigins, origin) {
			w.WriteHeader(http.StatusForbidden)

			return
		}

		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, QUERY, HEAD, POST, PUT, CONNECT, OPTIONS, TRACE, PATCH")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Origin")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length")
		w.Header().Set("Access-Control-Max-Age", "600")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)

			return
		}

		next.ServeHTTP(w, r)
	})
}
