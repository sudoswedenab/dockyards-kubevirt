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

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=dockyards.io,resources=organizations,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=organizations/status,verbs=patch

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

	conditions.MarkTrue(&dockyardsOrganization, dockyardsv1.ReadyCondition, dockyardsv1.ReadyReason, "")

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
