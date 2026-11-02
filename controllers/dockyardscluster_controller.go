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
	"github.com/sudoswedenab/dockyards-backend/api/apiutil"
	dyconfig "github.com/sudoswedenab/dockyards-backend/api/config"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters/status,verbs=patch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=workloads,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;patch;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtclusters,verbs=create;get;list;patch;watch

type DockyardsClusterReconciler struct {
	client.Client

	GatewayParentReference   gatewayapiv1.ParentReference
	DockyardsSystemNamespace string
	DockyardsConfig          *dyconfig.ConfigManager
	EnableWorkloadIngress    bool
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

	result, err = r.reconcileKubevirtCluster(ctx, &dockyardsCluster)
	if err != nil {
		return result, err
	}

	gatewayParentReference, err := resolveClusterGatewayParentReference(&dockyardsCluster, r.GatewayParentReference)
	if err != nil {
		return ctrl.Result{}, err
	}

	objectKey := client.ObjectKey{
		Name: string(gatewayParentReference.Name),
	}

	if gatewayParentReference.Namespace != nil {
		objectKey.Namespace = string(*gatewayParentReference.Namespace)
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

	result, err = r.reconcileTLSRoute(ctx, &dockyardsCluster, gatewayParentReference)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) reconcileKubevirtCluster(ctx context.Context, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	kubevirtCluster := providerv1.KubevirtCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsCluster.Name,
			Namespace: dockyardsCluster.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &kubevirtCluster, func() error {
		if kubevirtCluster.Labels == nil {
			kubevirtCluster.Labels = map[string]string{}
		}

		kubevirtCluster.Labels[dockyardsv1.LabelClusterName] = dockyardsCluster.Name
		kubevirtCluster.Labels[dockyardsv1.LabelOrganizationName] = dockyardsCluster.Labels[dockyardsv1.LabelOrganizationName]

		kubevirtCluster.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion:         dockyardsv1.GroupVersion.String(),
				Kind:               dockyardsv1.ClusterKind,
				Name:               dockyardsCluster.Name,
				UID:                dockyardsCluster.UID,
				BlockOwnerDeletion: new(true),
			},
		}

		kubevirtCluster.Spec.ControlPlaneServiceTemplate = providerv1.ControlPlaneServiceTemplate{
			Spec: providerv1.ServiceSpecTemplate{
				Type: corev1.ServiceTypeClusterIP,
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled kubevirt cluster", "result", operationResult)
	}

	capiCluster := clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dockyardsCluster.Name,
			Namespace: dockyardsCluster.Namespace,
		},
	}

	err = r.Get(ctx, client.ObjectKeyFromObject(&capiCluster), &capiCluster)
	if apierrors.IsNotFound(err) {
		conditions.MarkFalse(dockyardsCluster, CAPIClusterReconciledCondition, WaitingForCAPIClusterReason, "")

		return ctrl.Result{}, nil
	}

	if err != nil {
		conditions.MarkFalse(dockyardsCluster, CAPIClusterReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, err
	}

	patchHelper, err := patch.NewHelper(&capiCluster, r.Client)
	if err != nil {
		conditions.MarkFalse(dockyardsCluster, CAPIClusterReconciledCondition, ErrorReconcilingReason, "%s", err)

		return ctrl.Result{}, err
	}

	desiredInfrastructureRef := clusterv1.ContractVersionedObjectReference{
		APIGroup: providerv1.GroupVersion.Group,
		Kind:     "KubevirtCluster",
		Name:     dockyardsCluster.Name,
	}

	infrastructureRefPatched := false
	if !clusterInfrastructureRefMatches(capiCluster.Spec.InfrastructureRef, desiredInfrastructureRef) {
		capiCluster.Spec.InfrastructureRef = desiredInfrastructureRef

		err = patchHelper.Patch(ctx, &capiCluster)
		if err != nil {
			conditions.MarkFalse(dockyardsCluster, CAPIClusterReconciledCondition, ErrorReconcilingReason, "%s", err)

			return ctrl.Result{}, err
		}

		infrastructureRefPatched = true
	}

	ownerRefPatched := false
	updatedOwnerReferences := upsertOwnerReference(kubevirtCluster.OwnerReferences, metav1.OwnerReference{
		APIVersion:         clusterv1.GroupVersion.String(),
		Kind:               "Cluster",
		Name:               capiCluster.Name,
		UID:                capiCluster.UID,
		Controller:         new(true),
		BlockOwnerDeletion: new(true),
	})

	if !ownerReferencesEqual(kubevirtCluster.OwnerReferences, updatedOwnerReferences) {
		openStackClusterPatchHelper, err := patch.NewHelper(&kubevirtCluster, r.Client)
		if err != nil {
			conditions.MarkFalse(dockyardsCluster, CAPIClusterReconciledCondition, ErrorReconcilingReason, "%s", err)

			return ctrl.Result{}, err
		}

		kubevirtCluster.OwnerReferences = updatedOwnerReferences

		err = openStackClusterPatchHelper.Patch(ctx, &kubevirtCluster)
		if err != nil {
			conditions.MarkFalse(dockyardsCluster, CAPIClusterReconciledCondition, ErrorReconcilingReason, "%s", err)

			return ctrl.Result{}, err
		}

		ownerRefPatched = true
	}

	if infrastructureRefPatched || ownerRefPatched {
		logger.Info("patched capi/kubevirt cluster references", "capiInfrastructureRefPatched", infrastructureRefPatched, "kubevirtOwnerRefPatched", ownerRefPatched)
	}

	conditions.MarkTrue(dockyardsCluster, CAPIClusterReconciledCondition, ReconciledReason, "")

	return ctrl.Result{}, nil
}

func ownerReferencesEqual(current, desired []metav1.OwnerReference) bool {
	if len(current) != len(desired) {
		return false
	}

	for index := range current {
		if current[index] != desired[index] {
			return false
		}
	}

	return true
}

func upsertOwnerReference(ownerReferences []metav1.OwnerReference, desired metav1.OwnerReference) []metav1.OwnerReference {
	for index := range ownerReferences {
		current := ownerReferences[index]
		if current.APIVersion != desired.APIVersion || current.Kind != desired.Kind || current.Name != desired.Name {
			continue
		}

		ownerReferences[index] = desired

		return ownerReferences
	}

	return append(ownerReferences, desired)
}

func clusterInfrastructureRefMatches(current, desired clusterv1.ContractVersionedObjectReference) bool {
	return current.APIGroup == desired.APIGroup && current.Kind == desired.Kind && current.Name == desired.Name
}

func (r *DockyardsClusterReconciler) reconcileAPIEndpoint(_ context.Context, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	url := r.DockyardsConfig.GetValueOrDefault(dyconfig.KeyExternalURL, "")
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
	if dockyardsCluster.Spec.NoDefaultIngressProvider {
		conditions.MarkTrue(dockyardsCluster, IngressWorkloadReconciledCondition, NoDefaultIngressProviderReason, "")

		return ctrl.Result{}, nil
	}

	name := dockyardsCluster.Name + "-ingress-nginx"
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

	publicNamespace := r.DockyardsConfig.GetValueOrDefault(dyconfig.KeyPublicNamespace, "dockyards-public")

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

		if workload.Labels == nil {
			workload.Labels = map[string]string{}
		}
		workload.Labels[dockyardsv1.LabelClusterName] = dockyardsCluster.Name
		workload.Labels[dockyardsv1.LabelWorkloadName] = name
		workload.Labels[dockyardsv1.LabelWorkloadTemplateName] = "ingress-nginx"
		workload.Labels[dockyardsv1.LabelOrganizationName] = dockyardsCluster.Namespace

		workload.Spec.WorkloadTemplateRef = &corev1.TypedObjectReference{
			Kind:      dockyardsv1.WorkloadTemplateKind,
			Name:      "ingress-nginx",
			Namespace: &publicNamespace,
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

func (r *DockyardsClusterReconciler) reconcileTLSRoute(ctx context.Context, dockyardsCluster *dockyardsv1.Cluster, gatewayParentReference gatewayapiv1.ParentReference) (ctrl.Result, error) {
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
				gatewayParentReference,
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

func (r *DockyardsClusterReconciler) capiClusterToDockyardsCluster(_ context.Context, obj client.Object) []ctrl.Request {
	capiCluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}

	clusterName := capiCluster.Labels[dockyardsv1.LabelClusterName]
	if clusterName == "" {
		ownerReference, err := apiutil.FindOwnerReference(capiCluster, dockyardsv1.ClusterKind)
		if err == nil {
			clusterName = ownerReference.Name
		}
	}

	if clusterName == "" {
		clusterName = capiCluster.Name
	}

	return []ctrl.Request{{
		NamespacedName: types.NamespacedName{
			Namespace: capiCluster.Namespace,
			Name:      clusterName,
		},
	}}
}

func capiClusterWatchPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return true },
		UpdateFunc: func(e event.UpdateEvent) bool {
			oldCluster, oldOK := e.ObjectOld.(*clusterv1.Cluster)
			newCluster, newOK := e.ObjectNew.(*clusterv1.Cluster)
			if !oldOK || !newOK {
				return true
			}

			return oldCluster.Generation != newCluster.Generation
		},
	}
}

func (r *DockyardsClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)
	_ = gatewayapiv1.Install(scheme)
	_ = gatewayapiv1alpha2.Install(scheme)
	_ = providerv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(mgr).
		Named("dockyards/cluster").
		For(&dockyardsv1.Cluster{}).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.capiClusterToDockyardsCluster),
			builder.WithPredicates(capiClusterWatchPredicate()),
		).
		Complete(r)
	if err != nil {
		return err
	}

	return nil
}
