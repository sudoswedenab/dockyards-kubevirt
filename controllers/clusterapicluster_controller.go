package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;patch;watch
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tlsroutes,verbs=create;get;list;patch;watch
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

	result, err = r.reconcileKubevirtCluster(ctx, &cluster)
	if err != nil {
		return result, err
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
	_ = providerv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(m).For(&clusterv1.Cluster{}).Complete(r)
	if err != nil {
		return err
	}

	return nil
}
