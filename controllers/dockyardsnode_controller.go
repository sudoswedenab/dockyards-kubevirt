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
	"strings"

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=dockyards.io,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=nodes/status,verbs=patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtmachines,verbs=get;list;watch

type DockyardsNodeReconciler struct {
	client.Client
}

func (r *DockyardsNodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)

	var dockyardsNode dockyardsv1.Node
	err := r.Get(ctx, req.NamespacedName, &dockyardsNode)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if dockyardsNode.Spec.ProviderID == nil || !strings.HasPrefix(*dockyardsNode.Spec.ProviderID, "kubevirt://") {
		return ctrl.Result{}, nil
	}

	kubevirtMachineName := strings.TrimPrefix(*dockyardsNode.Spec.ProviderID, "kubevirt://")

	var kubevirtMachine providerv1.KubevirtMachine
	err = r.Get(ctx, client.ObjectKey{Name: kubevirtMachineName, Namespace: dockyardsNode.Namespace}, &kubevirtMachine)
	if err != nil {
		logger.Info("ignoring dockyards node without kubevirt machine")

		return ctrl.Result{}, nil
	}

	patchHelper, err := patch.NewHelper(&dockyardsNode, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		err := patchHelper.Patch(ctx, &dockyardsNode)
		if err != nil {
			result = ctrl.Result{}
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	cpu := resource.NewQuantity(int64(kubevirtMachine.Spec.VirtualMachineTemplate.Spec.Template.Spec.Domain.CPU.Cores), resource.DecimalSI)

	storage := resource.Quantity{}

	for _, dataVolumeTemplate := range kubevirtMachine.Spec.VirtualMachineTemplate.Spec.DataVolumeTemplates {
		storage.Add(*dataVolumeTemplate.Spec.PVC.Resources.Requests.Storage())
	}

	dockyardsNode.Status.Resources = corev1.ResourceList{
		corev1.ResourceCPU:     *cpu,
		corev1.ResourceMemory:  *kubevirtMachine.Spec.VirtualMachineTemplate.Spec.Template.Spec.Domain.Memory.Guest,
		corev1.ResourceStorage: storage,
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsNodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)
	_ = providerv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(mgr).For(&dockyardsv1.Node{}).Complete(r)
	if err != nil {
		return err
	}

	return nil
}
