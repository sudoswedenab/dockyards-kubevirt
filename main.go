package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"bitbucket.org/sudosweden/dockyards-kubevirt/controllers"
	"github.com/go-logr/logr"
	"github.com/spf13/pflag"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/cluster-api/controllers/remote"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func main() {
	var gatewayName string
	var gatewayNamespace string
	var metricsBindAddress string
	var dockyardsNamespace string
	pflag.StringVar(&gatewayName, "gateway-name", "", "gateway name")
	pflag.StringVar(&gatewayNamespace, "gateway-namespace", "", "gateway namespace")
	pflag.StringVar(&metricsBindAddress, "metrics-bind-address", "0", "metrics bind address")
	pflag.StringVar(&dockyardsNamespace, "dockyards-namespace", "dockyards-system", "dockyards namespace")
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

	opts := manager.Options{
		Metrics: server.Options{
			BindAddress: metricsBindAddress,
		},
	}

	mgr, err := ctrl.NewManager(cfg, opts)
	if err != nil {
		slogr.Error(err, "error creating manager")

		os.Exit(1)
	}

	tracker, err := remote.NewClusterCacheTracker(mgr, remote.ClusterCacheTrackerOptions{})
	if err != nil {
		slogr.Error(err, "error creating cluster cache tracker")

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
		Client: mgr.GetClient(),
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
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards cluster reconciler")

		os.Exit(1)
	}

	err = (&controllers.DockyardsWorkloadReconciler{
		Client:                 mgr.GetClient(),
		Tracker:                tracker,
		GatewayParentReference: gatewayParentReference,
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards deployment reconciler")
	}

	err = (&controllers.DockyardsNodeReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards node reconciler")
	}

	err = mgr.Start(ctx)
	if err != nil {
		slogr.Error(err, "error starting manager")

		os.Exit(1)
	}
}
