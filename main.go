package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"

	"bitbucket.org/sudosweden/dockyards-kubevirt/controllers"
	"github.com/go-logr/logr"
	"sigs.k8s.io/cluster-api/controllers/remote"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

func main() {
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

	mgr, err := ctrl.NewManager(cfg, manager.Options{})
	if err != nil {
		slogr.Error(err, "error creating manager")

		os.Exit(1)
	}

	tracker, err := remote.NewClusterCacheTracker(mgr, remote.ClusterCacheTrackerOptions{})
	if err != nil {
		slogr.Error(err, "error creating cluster cache tracker")

		os.Exit(1)
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
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr)
	if err != nil {
		slogr.Error(err, "error creating dockyards cluster reconciler")

		os.Exit(1)
	}

	err = (&controllers.DockyardsDeploymentReconciler{
		Client:  mgr.GetClient(),
		Tracker: tracker,
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
