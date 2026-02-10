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
	"errors"
	"net/netip"
	"strings"

	"github.com/sudoswedenab/dockyards-backend/api/apiutil"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/controllers/clustercache"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=services,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=workloads,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=create;get;list;patch;watch

type DockyardsWorkloadReconciler struct {
	client.Client

	controller             controller.Controller
	GatewayParentReference gatewayapiv1.ParentReference
	ClusterCache           clustercache.ClusterCache
}

func (r *DockyardsWorkloadReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	var dockyardsWorkload dockyardsv1.Workload
	err := r.Get(ctx, req.NamespacedName, &dockyardsWorkload)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterName, has := dockyardsWorkload.Labels[dockyardsv1.LabelClusterName]
	if !has {
		logger.Info("ignoring dockyards workload without cluster name label")

		return ctrl.Result{}, nil
	}

	var cluster clusterv1.Cluster
	err = r.Get(ctx, client.ObjectKey{Name: clusterName, Namespace: dockyardsWorkload.Namespace}, &cluster)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if apierrors.IsNotFound(err) {
		logger.Info("ignoring dockyards workload without cluster")

		return ctrl.Result{}, nil
	}

	if conditions.IsFalse(&cluster, clusterv1.ControlPlaneInitializedCondition) {
		logger.Info("ignoring cluster services until control plane is initialized")

		return ctrl.Result{}, nil
	}

	ownerCluster, err := apiutil.GetOwnerCluster(ctx, r.Client, &dockyardsWorkload)
	if err != nil {
		return ctrl.Result{}, err
	}

	if ownerCluster == nil {
		logger.Info("ignoring dockyards workload without dockyards cluster")

		return ctrl.Result{}, nil
	}

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
		logger.Info("ignoring dockyards workload without gateway")

		return ctrl.Result{}, nil
	}

	var gatewayIP string
	for _, address := range gateway.Status.Addresses {
		if *address.Type == gatewayapiv1.IPAddressType {
			addr, err := netip.ParseAddr(address.Value)
			if err != nil {
				continue
			}

			if !addr.Is4() {
				continue
			}

			gatewayIP = address.Value

			break
		}
	}

	if gatewayIP == "" {
		logger.Info("ignoring dockyards workload without gateway ip")

		return ctrl.Result{}, nil
	}

	err = r.ClusterCache.Watch(ctx, client.ObjectKeyFromObject(&cluster), clustercache.NewWatcher(clustercache.WatcherOptions{
		Name:         "workload-watchServices",
		Watcher:      r.controller,
		Kind:         &corev1.Service{},
		EventHandler: handler.EnqueueRequestsFromMapFunc(r.serviceToWorkload),
	}))
	if err != nil {
		if errors.Is(err, clustercache.ErrClusterNotConnected) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	remoteClient, err := r.ClusterCache.GetClient(ctx, client.ObjectKeyFromObject(&cluster))
	if err != nil {
		return ctrl.Result{}, err
	}

	matchingLabels := client.MatchingLabels{
		dockyardsv1.LabelWorkloadName: dockyardsWorkload.Name,
	}

	var serviceList corev1.ServiceList
	err = remoteClient.List(ctx, &serviceList, matchingLabels)
	if err != nil {
		return ctrl.Result{}, err
	}

	for _, remoteService := range serviceList.Items {
		if remoteService.Spec.Type != corev1.ServiceTypeLoadBalancer {
			continue
		}

		logger.Info("remote service", "type", remoteService.Spec.Type, "serviceName", remoteService.Name)

		if len(remoteService.Status.LoadBalancer.Ingress) != 1 || remoteService.Status.LoadBalancer.Ingress[0].IP != gatewayIP {
			logger.Info("service needs ip update", "serviceName", remoteService.Name)

			patch := client.MergeFrom(remoteService.DeepCopy())

			remoteService.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{
				{
					IP: gatewayIP,
				},
			}

			err := remoteClient.Status().Patch(ctx, &remoteService, patch)
			if err != nil {
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, nil
		}

		serviceName := dockyardsWorkload.Name + "-" + remoteService.Name

		service := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: dockyardsWorkload.Namespace,
			},
		}

		servicePorts := make([]corev1.ServicePort, len(remoteService.Spec.Ports))
		for i, servicePort := range remoteService.Spec.Ports {
			servicePorts[i] = corev1.ServicePort{
				Name:       servicePort.Name,
				Port:       servicePort.Port,
				TargetPort: intstr.FromInt32(servicePort.NodePort),
			}
		}

		operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &service, func() error {
			service.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: dockyardsv1.GroupVersion.String(),
					Kind:       dockyardsv1.WorkloadKind,
					Name:       dockyardsWorkload.Name,
					UID:        dockyardsWorkload.UID,
				},
			}

			service.Spec.Ports = servicePorts

			service.Spec.Selector = map[string]string{
				"cluster.x-k8s.io/cluster-name": cluster.Name,
				"cluster.x-k8s.io/role":         "worker",
			}

			return nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}

		if operationResult != controllerutil.OperationResultNone {
			logger.Info("reconciled service", "serviceName", serviceName, "result", operationResult)
		}

		hostnames := make([]gatewayapiv1.Hostname, len(ownerCluster.Status.DNSZones))

		for i, dnsZone := range ownerCluster.Status.DNSZones {
			hostname := "*." + strings.TrimSuffix(dnsZone, ".")

			hostnames[i] = gatewayapiv1.Hostname(hostname)
		}

		httpRoute := gatewayapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: dockyardsWorkload.Namespace,
			},
		}

		operationResult, err = controllerutil.CreateOrPatch(ctx, r.Client, &httpRoute, func() error {
			httpRoute.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: dockyardsv1.GroupVersion.String(),
					Kind:       dockyardsv1.WorkloadKind,
					Name:       dockyardsWorkload.Name,
					UID:        dockyardsWorkload.UID,
				},
			}

			httpRoute.Spec.Hostnames = hostnames

			httpRoute.Spec.Rules = []gatewayapiv1.HTTPRouteRule{
				{
					BackendRefs: []gatewayapiv1.HTTPBackendRef{
						{
							BackendRef: gatewayapiv1.BackendRef{
								BackendObjectReference: gatewayapiv1.BackendObjectReference{
									Name: gatewayapiv1.ObjectName(serviceName),
									Port: ptr.To(gatewayapiv1.PortNumber(80)),
								},
							},
						},
					},
				},
			}

			httpRoute.Spec.ParentRefs = []gatewayapiv1.ParentReference{
				r.GatewayParentReference,
			}

			return nil
		})
		if err != nil {
			return ctrl.Result{}, err
		}

		logger.Info("reconciled http route", "result", operationResult)

		tlsRoute := gatewayapiv1alpha2.TLSRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: dockyardsWorkload.Namespace,
			},
		}

		operationResult, err = controllerutil.CreateOrPatch(ctx, r.Client, &tlsRoute, func() error {
			tlsRoute.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: dockyardsv1.GroupVersion.String(),
					Kind:       dockyardsv1.WorkloadKind,
					Name:       dockyardsWorkload.Name,
					UID:        dockyardsWorkload.UID,
				},
			}

			tlsRoute.Spec.CommonRouteSpec = gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					r.GatewayParentReference,
				},
			}

			tlsRoute.Spec.Hostnames = hostnames

			tlsRoute.Spec.Rules = []gatewayapiv1alpha2.TLSRouteRule{
				{
					BackendRefs: []gatewayapiv1.BackendRef{
						{
							BackendObjectReference: gatewayapiv1.BackendObjectReference{
								Name: gatewayapiv1.ObjectName(serviceName),
								Port: ptr.To(gatewayapiv1.PortNumber(443)),
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

	}

	return ctrl.Result{}, nil
}

func (r *DockyardsWorkloadReconciler) serviceToWorkload(ctx context.Context, obj client.Object) []ctrl.Request {
	logger := ctrl.LoggerFrom(ctx)

	service, ok := obj.(*corev1.Service)
	if !ok {
		panic("expected service")
	}

	if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
		logger.Info("ignoring service", "type", service.Spec.Type, "serviceName", service.Name)

		return nil
	}

	logger.Info("service", "type", service.Spec.Type, "serviceName", service.Name)

	workloadName, has := service.Labels[dockyardsv1.LabelWorkloadName]
	if !has {
		logger.Info("ignoring service without workload name label", "serviceName", service.Name)

		return nil
	}

	workloadNamespace, has := service.Labels["kustomize.toolkit.fluxcd.io/namespace"]
	if !has {
		logger.Info("ignoring service without kustomize namespace label", "serviceName", service.Name)

		return nil
	}

	return []ctrl.Request{
		{
			NamespacedName: client.ObjectKey{
				Name:      workloadName,
				Namespace: workloadNamespace,
			},
		},
	}
}

func (r *DockyardsWorkloadReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)

	c, err := ctrl.NewControllerManagedBy(mgr).For(&dockyardsv1.Workload{}).Build(r)
	if err != nil {
		return err
	}

	r.controller = c

	return nil
}
