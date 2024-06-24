package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/utils/ptr"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gatewayapiv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;patch;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=kubevirtclusters,verbs=create;get;list;patch;watch

type ClusterAPIClusterReconciler struct {
	client.Client
}

func (r *ClusterAPIClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
	var cluster clusterv1.Cluster
	err := r.Get(ctx, req.NamespacedName, &cluster)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(&cluster, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	defer func() {
		err := patchHelper.Patch(ctx, &cluster)
		if err != nil {
			result = ctrl.Result{}
			reterr = kerrors.NewAggregate([]error{reterr, err})
		}
	}()

	result, err = r.reconcileTLSRoute(ctx, &cluster)
	if err != nil {
		return result, err
	}

	result, err = r.reconcileKubevirtCluster(ctx, &cluster)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *ClusterAPIClusterReconciler) reconcileTLSRoute(ctx context.Context, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	var service corev1.Service
	err := r.Get(ctx, client.ObjectKey{Name: "dockyards-public", Namespace: "dockyards"}, &service)
	if err != nil {
		return ctrl.Result{}, err
	}

	if service.Spec.Type != corev1.ServiceTypeExternalName || service.Spec.ExternalName == "" {
		logger.Info("ignoring service")

		return ctrl.Result{}, nil
	}

	externalName := cluster.Namespace + "-" + cluster.Name + "." + service.Spec.ExternalName

	tlsRoute := gatewayapiv1alpha2.TLSRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &tlsRoute, func() error {
		tlsRoute.Spec.CommonRouteSpec = gatewayapiv1.CommonRouteSpec{
			ParentRefs: []gatewayapiv1.ParentReference{
				{
					Name:      gatewayapiv1.ObjectName("dockyards-public"),
					Namespace: ptr.To(gatewayapiv1.Namespace("dockyards")),
				},
			},
		}

		tlsRoute.Spec.Hostnames = []gatewayapiv1.Hostname{
			gatewayapiv1.Hostname(externalName),
		}

		tlsRoute.Spec.Rules = []gatewayapiv1alpha2.TLSRouteRule{
			{
				BackendRefs: []gatewayapiv1.BackendRef{
					{
						BackendObjectReference: gatewayapiv1.BackendObjectReference{
							Name: gatewayapiv1.ObjectName(cluster.Name + "-lb"),
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

func (r *ClusterAPIClusterReconciler) reconcileKubevirtCluster(ctx context.Context, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	kubevirtCluster := providerv1.KubevirtCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: cluster.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &kubevirtCluster, func() error {
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

	if cluster.Spec.InfrastructureRef == nil {
		cluster.Spec.InfrastructureRef = &corev1.ObjectReference{
			APIVersion: providerv1.GroupVersion.String(),
			Kind:       "KubevirtCluster",
			Name:       kubevirtCluster.Name,
			Namespace:  kubevirtCluster.Namespace,
		}
	}

	return ctrl.Result{}, nil
}

func (r *ClusterAPIClusterReconciler) SetupWithManager(m ctrl.Manager) error {
	scheme := m.GetScheme()

	_ = clusterv1.AddToScheme(scheme)
	_ = gatewayapiv1.Install(scheme)
	_ = gatewayapiv1alpha2.Install(scheme)
	_ = providerv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(m).For(&clusterv1.Cluster{}).Complete(r)
	if err != nil {
		return err
	}

	return nil
}
