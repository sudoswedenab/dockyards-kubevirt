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
	"bytes"
	"context"
	"encoding/json"
	"net/netip"
	"strings"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	dyconfig "github.com/sudoswedenab/dockyards-backend/api/config"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters/status,verbs=patch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=workloads,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch

type DockyardsClusterReconciler struct {
	client.Client

	GatewayParentReference gatewayapiv1.ParentReference
	DockyardsNamespace     string
	DockyardsConfig        *dyconfig.DockyardsConfig
	EnableWorkloadIngress  bool
}

func (r *DockyardsClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)

	var dockyardsCluster dockyardsv1.Cluster
	err := r.Get(ctx, req.NamespacedName, &dockyardsCluster)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(&dockyardsCluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		err := patchHelper.Patch(ctx, &dockyardsCluster)
		if err != nil {
			result = ctrl.Result{}
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	objectKey := client.ObjectKey{
		Name: string(r.GatewayParentReference.Name),
	}

	if r.GatewayParentReference.Namespace != nil {
		objectKey.Namespace = string(*r.GatewayParentReference.Namespace)
	}

	var gateway gatewayapiv1.Gateway
	err = r.Get(ctx, objectKey, &gateway)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if apierrors.IsNotFound(err) {
		logger.Info("ignoring cluster without gateway")

		return ctrl.Result{}, nil
	}

	result, err = r.reconcileAPIEndpoint(ctx, &dockyardsCluster)
	if err != nil {
		return result, err
	}

	result, err = r.reconcileIngressNginx(ctx, &dockyardsCluster, &gateway)
	if err != nil {
		return result, err
	}

	result, err = r.reconcileTLSRoute(ctx, &dockyardsCluster)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) reconcileAPIEndpoint(_ context.Context, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	url := r.DockyardsConfig.GetConfigKey(dyconfig.KeyExternalURL, "")
	hostname := strings.TrimPrefix(url, "http://")
	hostname = strings.TrimPrefix(hostname, "https://")

	if hostname == "" {
		conditions.MarkFalse(dockyardsCluster, APIEndpointReconciledCondition, WaitingForListenerHostnameReason, "")

		return ctrl.Result{}, nil
	}

	host := dockyardsCluster.Namespace + "-" + dockyardsCluster.Name + "." + hostname

	dockyardsCluster.Status.APIEndpoint = dockyardsv1.ClusterAPIEndpoint{
		Host: host,
		Port: 6443,
	}

	conditions.MarkTrue(dockyardsCluster, APIEndpointReconciledCondition, ReconciledReason, "")

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) reconcileIngressNginx(ctx context.Context, dockyardsCluster *dockyardsv1.Cluster, gateway *gatewayapiv1.Gateway) (ctrl.Result, error) {
	input := map[string]any{}

	if r.EnableWorkloadIngress {
		var gatewayIP string
		for _, address := range gateway.Status.Addresses {
			if address.Type == nil || *address.Type != gatewayapiv1.IPAddressType {
				continue
			}

			addr, err := netip.ParseAddr(address.Value)
			if err != nil {
				continue
			}

			if !addr.Is4() {
				continue
			}

			gatewayIP = address.Value
		}

		if gatewayIP == "" {
			conditions.MarkFalse(dockyardsCluster, IngressWorkloadReconciledCondition, WaitingForGatewayAddressReason, "")

			return ctrl.Result{}, nil
		}
		input["service"] = map[string]any{
			"loadBalancerIP": gatewayIP,
		}
	}

	name := dockyardsCluster.Name + "-ingress-nginx"

	raw, err := json.Marshal(input)
	if err != nil {
		conditions.MarkFalse(dockyardsCluster, IngressWorkloadReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, err
	}

	workload := dockyardsv1.Workload{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: dockyardsCluster.Namespace,
		},
	}

	_, err = controllerutil.CreateOrPatch(ctx, r.Client, &workload, func() error {
		workload.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: dockyardsv1.GroupVersion.String(),
				Kind:       dockyardsv1.ClusterKind,
				Name:       dockyardsCluster.Name,
				UID:        dockyardsCluster.UID,
			},
		}

		workload.Spec.ClusterComponent = true
		workload.Spec.Provenience = dockyardsv1.ProvenienceDockyards
		workload.Spec.TargetNamespace = "ingress-nginx"

		workload.Labels = map[string]string{
			dockyardsv1.LabelClusterName: dockyardsCluster.Name,
		}

		workload.Spec.WorkloadTemplateRef = &corev1.TypedObjectReference{
			Kind:      dockyardsv1.WorkloadTemplateKind,
			Name:      "ingress-nginx",
			Namespace: &r.DockyardsNamespace,
		}

		if !bytes.Equal(raw, []byte("{}")) {
			workload.Spec.Input = &apiextensionsv1.JSON{
				Raw: raw,
			}
		}

		return nil
	})
	if err != nil {
		conditions.MarkFalse(dockyardsCluster, IngressWorkloadReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, err
	}

	conditions.MarkTrue(dockyardsCluster, IngressWorkloadReconciledCondition, dockyardsv1.ReadyCondition, "")

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) reconcileTLSRoute(ctx context.Context, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	if !dockyardsCluster.Status.APIEndpoint.IsValid() {
		logger.Info("ignoring tls route for cluster without valid api endpoint")

		return ctrl.Result{}, nil
	}

	tlsRoute := gatewayapiv1alpha2.TLSRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsCluster.Name,
			Namespace: dockyardsCluster.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &tlsRoute, func() error {
		tlsRoute.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: dockyardsv1.GroupVersion.String(),
				Kind:       dockyardsv1.ClusterKind,
				Name:       dockyardsCluster.Name,
				UID:        dockyardsCluster.UID,
			},
		}

		tlsRoute.Labels = map[string]string{
			dockyardsv1.LabelClusterName: dockyardsCluster.Name,
		}

		tlsRoute.Spec.CommonRouteSpec = gatewayapiv1.CommonRouteSpec{
			ParentRefs: []gatewayapiv1.ParentReference{
				r.GatewayParentReference,
			},
		}

		tlsRoute.Spec.Hostnames = []gatewayapiv1.Hostname{
			gatewayapiv1.Hostname(dockyardsCluster.Status.APIEndpoint.Host),
		}

		tlsRoute.Spec.Rules = []gatewayapiv1alpha2.TLSRouteRule{
			{
				BackendRefs: []gatewayapiv1.BackendRef{
					{
						BackendObjectReference: gatewayapiv1.BackendObjectReference{
							Name: gatewayapiv1.ObjectName(dockyardsCluster.Name + "-lb"),
							Port: ptr.To(gatewayapiv1.PortNumber(6443)),
						},
					},
				},
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled tls route", "result", operationResult)
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)
	_ = gatewayapiv1.Install(scheme)
	_ = gatewayapiv1alpha2.Install(scheme)

	err := ctrl.NewControllerManagedBy(mgr).
		Named("dockyards/cluster").
		For(&dockyardsv1.Cluster{}).
		Complete(r)
	if err != nil {
		return err
	}

	return nil
}
