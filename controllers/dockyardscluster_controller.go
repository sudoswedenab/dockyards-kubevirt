package controllers

import (
	"context"
	"strings"

	dockyardsv1 "bitbucket.org/sudosweden/dockyards-backend/pkg/api/v1alpha2"
	"github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/yaml"
)

// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=clusters/status,verbs=patch
// +kubebuilder:rbac:groups=dockyards.io,resources=deployments,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=kustomizedeployments,verbs=create;get;list;patch;watch

type DockyardsClusterReconciler struct {
	client.Client
}

func (r *DockyardsClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, reterr error) {
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

	result, err = r.reconcileAPIEndpoint(ctx, &dockyardsCluster)
	if err != nil {
		return result, err
	}

	result, err = r.reconcileIngressNginx(ctx, &dockyardsCluster)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) reconcileAPIEndpoint(ctx context.Context, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	var service corev1.Service
	err := r.Get(ctx, client.ObjectKey{Name: "dockyards-public", Namespace: "dockyards"}, &service)
	if client.IgnoreNotFound(err) != nil {
		return ctrl.Result{}, err
	}

	if apierrors.IsNotFound(err) {
		logger.Info("ignoring cluster without public service")

		return ctrl.Result{}, nil
	}

	if service.Spec.Type != corev1.ServiceTypeExternalName || service.Spec.ExternalName == "" {
		logger.Info("ignoring service")

		return ctrl.Result{}, nil
	}

	externalName := dockyardsCluster.Namespace + "-" + dockyardsCluster.Name + "." + service.Spec.ExternalName

	dockyardsCluster.Status.APIEndpoint = dockyardsv1.ClusterAPIEndpoint{
		Host: externalName,
		Port: 6443,
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) reconcileIngressNginx(ctx context.Context, dockyardsCluster *dockyardsv1.Cluster) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	name := dockyardsCluster.Name + "-ingress-nginx"

	deployment := dockyardsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: dockyardsCluster.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &deployment, func() error {
		deployment.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: dockyardsv1.GroupVersion.String(),
				Kind:       dockyardsv1.ClusterKind,
				Name:       dockyardsCluster.Name,
				UID:        dockyardsCluster.UID,
			},
		}

		deployment.Spec.ClusterComponent = true
		deployment.Spec.TargetNamespace = "ingress-nginx"

		if deployment.Labels == nil {
			deployment.Labels = make(map[string]string)
		}

		deployment.Labels[dockyardsv1.LabelClusterName] = dockyardsCluster.Name

		deployment.Spec.DeploymentRefs = []corev1.TypedLocalObjectReference{
			{
				APIGroup: &dockyardsv1.GroupVersion.Group,
				Kind:     dockyardsv1.KustomizeDeploymentKind,
				Name:     name,
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled deployment", "result", operationResult)

	kustomizationYAML, err := yaml.Marshal(map[string]any{
		"apiVersion": "kustomize.config.k8s.io/v1beta1",
		"kind":       "Kustomization",
		"patches": []map[string]any{
			{
				"patch": strings.Join([]string{
					"- op: replace",
					"  path: /kind",
					"  value: DaemonSet",
				}, "\n"),
				"target": map[string]string{
					"group":   "apps",
					"version": "v1",
					"kind":    "Deployment",
					"name":    "ingress-nginx-controller",
				},
			},
		},
		"resources": []string{
			"github.com/kubernetes/ingress-nginx/deploy/static/provider/cloud?ref=controller-v1.10.1",
		},
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	kustomizeDeployment := dockyardsv1.KustomizeDeployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: dockyardsCluster.Namespace,
		},
	}

	operationResult, err = controllerutil.CreateOrPatch(ctx, r.Client, &kustomizeDeployment, func() error {
		kustomizeDeployment.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: dockyardsv1.GroupVersion.String(),
				Kind:       dockyardsv1.DeploymentKind,
				Name:       deployment.Name,
				UID:        deployment.UID,
			},
		}

		if kustomizeDeployment.Labels == nil {
			kustomizeDeployment.Labels = make(map[string]string)
		}

		kustomizeDeployment.Labels[dockyardsv1.LabelClusterName] = dockyardsCluster.Name
		kustomizeDeployment.Labels[dockyardsv1.LabelDeploymentName] = deployment.Name

		kustomizeDeployment.Spec.Kustomize = map[string][]byte{
			"kustomization.yaml": kustomizationYAML,
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled kustomize deployment", "result", operationResult)

	return ctrl.Result{}, nil
}

func (r *DockyardsClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(mgr).For(&dockyardsv1.Cluster{}).Complete(r)
	if err != nil {
		return err
	}

	return nil
}
