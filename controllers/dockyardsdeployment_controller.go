package controllers

import (
	"context"

	"bitbucket.org/sudosweden/dockyards-backend/pkg/api/apiutil"
	dockyardsv1 "bitbucket.org/sudosweden/dockyards-backend/pkg/api/v1alpha2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/remote"
	"sigs.k8s.io/cluster-api/util/conditions"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

type DockyardsDeploymentReconciler struct {
	client.Client
	Tracker    *remote.ClusterCacheTracker
	controller controller.Controller
}

func (r *DockyardsDeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	var dockyardsDeployment dockyardsv1.Deployment
	err := r.Get(ctx, req.NamespacedName, &dockyardsDeployment)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	clusterName, has := dockyardsDeployment.Labels[dockyardsv1.LabelClusterName]
	if !has || true {
		logger.Info("ignoring dockyards deployment without cluster name label")

		return ctrl.Result{}, nil
	}

	var cluster clusterv1.Cluster
	err = r.Get(ctx, client.ObjectKey{Name: clusterName, Namespace: dockyardsDeployment.Namespace}, &cluster)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if apierrors.IsNotFound(err) {
		logger.Info("ignoring dockyards deployment without cluster")

		return ctrl.Result{}, nil
	}

	if conditions.IsFalse(&cluster, clusterv1.ControlPlaneInitializedCondition) {
		logger.Info("ignoring cluster services until control plane is initialized")

		return ctrl.Result{}, nil
	}

	ownerCluster, err := apiutil.GetOwnerCluster(ctx, r.Client, &dockyardsDeployment)
	if err != nil {
		return ctrl.Result{}, err
	}

	if ownerCluster == nil {
		logger.Info("ignoring dockyards deployment without dockyards cluster")

		return ctrl.Result{}, nil
	}

	var gateway gatewayapiv1.Gateway
	err = r.Get(ctx, client.ObjectKey{Name: "dockyards-public", Namespace: "dockyards"}, &gateway)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if apierrors.IsNotFound(err) {
		logger.Info("ignoring dockyards deployment without gateway")

		return ctrl.Result{}, nil
	}

	var gatewayIP string
	for _, address := range gateway.Status.Addresses {
		if *address.Type == gatewayapiv1.IPAddressType {
			gatewayIP = address.Value

			break
		}
	}

	if gatewayIP == "" {
		logger.Info("ignoring dockyards deployment without gateway ip")

		return ctrl.Result{}, nil
	}

	c := client.ObjectKey{
		Name:      cluster.Name + "-external",
		Namespace: cluster.Namespace,
	}

	err = r.watchClusterServices(ctx, &cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	remoteClient, err := r.Tracker.GetClient(ctx, c)
	if err != nil {
		return ctrl.Result{}, err
	}

	matchingLabels := client.MatchingLabels{
		dockyardsv1.LabelDeploymentName: dockyardsDeployment.Name,
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

		serviceName := dockyardsDeployment.Name + "-" + remoteService.Name

		service := corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: dockyardsDeployment.Namespace,
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
					Kind:       dockyardsv1.DeploymentKind,
					Name:       dockyardsDeployment.Name,
					UID:        dockyardsDeployment.UID,
				},
			}

			service.Spec.Ports = servicePorts

			service.Spec.Selector = map[string]string{
				"cluster.x-k8s.io/cluster-name": "test-j9fpt",
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
			hostname := "*." + dnsZone

			hostnames[i] = gatewayapiv1.Hostname(hostname)
		}

		httpRoute := gatewayapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      serviceName,
				Namespace: dockyardsDeployment.Namespace,
			},
		}

		operationResult, err = controllerutil.CreateOrPatch(ctx, r.Client, &httpRoute, func() error {
			httpRoute.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: dockyardsv1.GroupVersion.String(),
					Kind:       dockyardsv1.DeploymentKind,
					Name:       dockyardsDeployment.Name,
					UID:        dockyardsDeployment.UID,
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
				{
					Namespace: ptr.To(gatewayapiv1.Namespace("dockyards")),
					Name:      gatewayapiv1.ObjectName("dockyards-public"),
				},
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
				Namespace: dockyardsDeployment.Namespace,
			},
		}

		operationResult, err = controllerutil.CreateOrPatch(ctx, r.Client, &tlsRoute, func() error {
			tlsRoute.OwnerReferences = []metav1.OwnerReference{
				{
					APIVersion: dockyardsv1.GroupVersion.String(),
					Kind:       dockyardsv1.DeploymentKind,
					Name:       dockyardsDeployment.Name,
					UID:        dockyardsDeployment.UID,
				},
			}

			tlsRoute.Spec.CommonRouteSpec = gatewayapiv1.CommonRouteSpec{
				ParentRefs: []gatewayapiv1.ParentReference{
					{
						Name:      gatewayapiv1.ObjectName("dockyards-public"),
						Namespace: ptr.To(gatewayapiv1.Namespace("dockyards")),
					},
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

func (r *DockyardsDeploymentReconciler) watchClusterServices(ctx context.Context, cluster *clusterv1.Cluster) error {
	if r.Tracker == nil {
		return nil
	}

	c := client.ObjectKey{
		Name:      cluster.Name + "-external",
		Namespace: cluster.Namespace,
	}

	return r.Tracker.Watch(ctx, remote.WatchInput{
		Name: "deployment-watchServices",
		//Cluster:      client.ObjectKeyFromObject(cluster),
		Cluster:      c,
		Watcher:      r.controller,
		Kind:         &corev1.Service{},
		EventHandler: handler.EnqueueRequestsFromMapFunc(r.serviceToDeployment),
	})
}

func (r *DockyardsDeploymentReconciler) serviceToDeployment(ctx context.Context, obj client.Object) []ctrl.Request {
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

	deploymentName, has := service.Labels[dockyardsv1.LabelDeploymentName]
	if !has {
		logger.Info("ignoring service without deployment name label", "serviceName", service.Name)

		return nil
	}

	deploymentNamespace, has := service.Labels["kustomize.toolkit.fluxcd.io/namespace"]
	if !has {
		logger.Info("ignoring service without kustomize namespace label", "serviceName", service.Name)

		return nil
	}

	return []ctrl.Request{
		{
			NamespacedName: client.ObjectKey{
				Name:      deploymentName,
				Namespace: deploymentNamespace,
			},
		},
	}
}

func (r *DockyardsDeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)

	c, err := ctrl.NewControllerManagedBy(mgr).For(&dockyardsv1.Deployment{}).Build(r)
	if err != nil {
		return err
	}

	r.controller = c

	return nil
}
