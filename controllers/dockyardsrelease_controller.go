package controllers

import (
	"context"

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datasources,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes/source,verbs=create
// +kubebuilder:rbac:groups=cdi.kubevirt.io,resources=datavolumes,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=dockyards.io,resources=releases,verbs=get;list;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=create;get;list;patch;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=create;get;list;patch;watch

type DockyardsReleaseReconciler struct {
	client.Client

	DataVolumeStorageClassName *string
}

func (r *DockyardsReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var release dockyardsv1.Release
	err := r.Get(ctx, req.NamespacedName, &release)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if release.Spec.Type != dockyardsv1.ReleaseTypeTalosInstaller {
		return ctrl.Result{}, nil
	}

	if release.Status.LatestURL == nil {
		return ctrl.Result{}, nil
	}

	result, err := r.reconcileDataVolume(ctx, &release)
	if err != nil {
		return result, err
	}

	result, err = r.reconcileAccessControl(ctx, &release)
	if err != nil {
		return result, err
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsReleaseReconciler) reconcileDataVolume(ctx context.Context, release *dockyardsv1.Release) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	dataVolume := cdiv1.DataVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.Name + "-" + release.Status.LatestVersion,
			Namespace: release.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &dataVolume, func() error {
		dataVolume.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: dockyardsv1.GroupVersion.String(),
				Kind:       dockyardsv1.ReleaseKind,
				Name:       release.Name,
				UID:        release.UID,
			},
		}

		if dataVolume.Annotations == nil {
			dataVolume.Annotations = make(map[string]string)
		}

		dataVolume.Annotations["cdi.kubevirt.io/storage.bind.immediate.requested"] = ""

		if dataVolume.Labels == nil {
			dataVolume.Labels = make(map[string]string)
		}

		dataVolume.Labels[dockyardsv1.LabelReleaseName] = release.Name

		dataVolume.Spec.Storage = &cdiv1.StorageSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
			VolumeMode: ptr.To(corev1.PersistentVolumeBlock),
		}

		if r.DataVolumeStorageClassName != nil {
			dataVolume.Spec.Storage.StorageClassName = r.DataVolumeStorageClassName
		}

		dataVolume.Spec.Source = &cdiv1.DataVolumeSource{
			HTTP: &cdiv1.DataVolumeSourceHTTP{
				URL: *release.Status.LatestURL,
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled data volume", "name", dataVolume.Name)
	}

	dataSource := cdiv1.DataSource{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.Name,
			Namespace: release.Namespace,
		},
	}

	operationResult, err = controllerutil.CreateOrPatch(ctx, r.Client, &dataSource, func() error {
		if dataSource.Labels == nil {
			dataSource.Labels = make(map[string]string)
		}

		dataSource.Labels[dockyardsv1.LabelReleaseName] = release.Name

		dataSource.Spec.Source.PVC = &cdiv1.DataVolumeSourcePVC{
			Name:      dataVolume.Name,
			Namespace: dataVolume.Namespace,
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled data source")
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsReleaseReconciler) reconcileAccessControl(ctx context.Context, release *dockyardsv1.Release) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	role := rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.Name,
			Namespace: release.Namespace,
		},
	}

	operationResult, err := controllerutil.CreateOrPatch(ctx, r.Client, &role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{
					cdiv1.SchemeGroupVersion.Group,
				},
				Resources: []string{
					"datavolumes/source",
				},
				Verbs: []string{
					"create",
				},
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled role", "result", operationResult)
	}

	roleBinding := rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      release.Name,
			Namespace: release.Namespace,
		},
	}

	operationResult, err = controllerutil.CreateOrPatch(ctx, r.Client, &roleBinding, func() error {
		roleBinding.RoleRef = rbacv1.RoleRef{
			Kind: "Role",
			Name: role.Name,
		}

		roleBinding.Subjects = []rbacv1.Subject{
			{
				APIGroup: rbacv1.GroupName,
				Kind:     rbacv1.GroupKind,
				Name:     "system:serviceaccounts",
			},
		}

		return nil
	})
	if err != nil {
		return ctrl.Result{}, err
	}

	if operationResult != controllerutil.OperationResultNone {
		logger.Info("reconciled role binding", "result", operationResult)
	}

	return ctrl.Result{}, nil
}

func (r *DockyardsReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	scheme := mgr.GetScheme()

	_ = dockyardsv1.AddToScheme(scheme)
	_ = cdiv1.AddToScheme(scheme)

	err := ctrl.NewControllerManagedBy(mgr).
		Named("dockyards/release").
		For(&dockyardsv1.Release{}).
		Owns(&cdiv1.DataVolume{}).
		Complete(r)
	if err != nil {
		return err
	}

	return nil
}
