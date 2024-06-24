package controllers

import (
	"context"

	dockyardsv1 "bitbucket.org/sudosweden/dockyards-backend/pkg/api/v1alpha2"
	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type DockyardsOrganizationReconciler struct {
	client.Client
}

func (r *DockyardsOrganizationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)

	var dockyardsOrganization dockyardsv1.Organization
	err := r.Get(ctx, req.NamespacedName, &dockyardsOrganization)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !dockyardsOrganization.DeletionTimestamp.IsZero() {
		logger.Info("ignoring deleted dockyards organization")

		return ctrl.Result{}, nil
	}

	if dockyardsOrganization.Spec.SkipAutoAssign {
		logger.Info("ignoring skip auto assign organization")

		return ctrl.Result{}, nil
	}

	if conditions.IsTrue(&dockyardsOrganization, dockyardsv1.ReadyCondition) {
		logger.Info("ignoring ready organization")

		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(&dockyardsOrganization, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		err := patchHelper.Patch(ctx, &dockyardsOrganization)
		if err != nil {
			result = ctrl.Result{}
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	conditions.MarkTrue(&dockyardsOrganization, dockyardsv1.ReadyCondition, "KubevirtOrganizationReady", "")

	return ctrl.Result{}, nil
}

func (r *DockyardsOrganizationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(mgr).For(&dockyardsv1.Organization{}).Complete(r)
	if err != nil {
		return err
	}

	return nil
}
