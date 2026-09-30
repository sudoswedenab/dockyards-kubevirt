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

	kubevirtv1 "kubevirt.io/api/core/v1"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// +kubebuilder:rbac:groups=kubevirt.io,resources=virtualmachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=virtualmachineinstances,verbs=create;get;list;watch;patch;update

type KubevirtVirtualMachineReconciler struct {
	client.Client
}

func (r *KubevirtVirtualMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var vm kubevirtv1.VirtualMachine
	err := r.Get(ctx, req.NamespacedName, &vm)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	result, err := r.reconcileDockyardsVirtualMachine(ctx, &vm)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *KubevirtVirtualMachineReconciler) reconcileDockyardsVirtualMachine(ctx context.Context, vm *kubevirtv1.VirtualMachine) (ctrl.Result, error) {
	var vmi dockyardsv1.VirtualMachineInstance
	vmi.Name = vm.Name
	vmi.Namespace = vm.Namespace
	_, err := controllerutil.CreateOrUpdate(ctx, r, &vmi, func() error {
		vmi.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: vm.APIVersion,
				Kind: vm.Kind,
				Name: vm.Name,
				UID: vm.UID,
			},
		}
		if vmi.Labels == nil {
			vmi.Labels = map[string]string{}
		}
		vmi.Labels[dockyardsv1.LabelProviderName] = "kubevirt"
		vmi.Labels[dockyardsv1.LabelVirtualMachineName] = vm.Name
		vmi.Status.Created = vm.Status.Created
		vmi.Status.Ready = vm.Status.Ready
		vmi.Status.PrintableStatus = string(vm.Status.PrintableStatus)
		vmi.Status.Conditions = make([]metav1.Condition, 0, len(vm.Status.Conditions))
		for _, cond := range vm.Status.Conditions {
			c := metav1.Condition{
				Type: string(cond.Type),
				Status: metav1.ConditionStatus(cond.Status),
				LastTransitionTime: cond.LastTransitionTime,
				Reason: cond.Reason,
				Message: cond.Message,
			}
			vmi.Status.Conditions = append(vmi.Status.Conditions, c)
		}
		return nil
	})

	return ctrl.Result{}, err
}

func (r *KubevirtVirtualMachineReconciler) SetupWithManager(m ctrl.Manager) error {
	scheme := m.GetScheme()

	_ = kubevirtv1.AddToScheme(scheme)
	_ = dockyardsv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(m).For(&kubevirtv1.VirtualMachine{}).Complete(r)
	if err != nil {
		return err
	}

	return nil
}
