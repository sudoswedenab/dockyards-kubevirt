package mockcrds

import (
	dockyardsv1 "bitbucket.org/sudosweden/dockyards-backend/pkg/api/v1alpha3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
)

var (
	DockyardsNodePool       = mockCRD(dockyardsv1.NodePoolKind, "nodepools", dockyardsv1.GroupVersion.Group, dockyardsv1.GroupVersion.Version)
	DockyardsRelease        = mockCRD(dockyardsv1.ReleaseKind, "releases", dockyardsv1.GroupVersion.Group, dockyardsv1.GroupVersion.Version)
	KubevirtMachineTemplate = mockCRD("KubevirtMachineTemplate", "kubevirtmachinetemplates", providerv1.GroupVersion.Group, providerv1.GroupVersion.Version)
	CDIDataVolume           = mockCRD("DataVolume", "datavolumes", cdiv1.CDIGroupVersionKind.Group, cdiv1.CDIGroupVersionKind.Version)
	CDIDataSource           = mockCRD("DataSource", "datasources", cdiv1.CDIGroupVersionKind.Group, cdiv1.CDIGroupVersionKind.Version)

	CRDs = []*apiextensionsv1.CustomResourceDefinition{
		DockyardsNodePool,
		DockyardsRelease,
		KubevirtMachineTemplate,
		CDIDataVolume,
		CDIDataSource,
	}
)

func mockCRD(kind, plural, group, version string) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{
			APIVersion: apiextensionsv1.SchemeGroupVersion.String(),
			Kind:       "CustomResourceDefinition",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name: plural + "." + group,
		},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: group,
			Scope: apiextensionsv1.NamespaceScoped,
			Names: apiextensionsv1.CustomResourceDefinitionNames{
				Plural: plural,
				Kind:   kind,
			},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{
					Name:    version,
					Served:  true,
					Storage: true,
					Subresources: &apiextensionsv1.CustomResourceSubresources{
						Status: &apiextensionsv1.CustomResourceSubresourceStatus{},
					},
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"spec": {
									Type:                   "object",
									XPreserveUnknownFields: ptr.To(true),
								},
								"status": {
									Type:                   "object",
									XPreserveUnknownFields: ptr.To(true),
								},
							},
						},
					},
				},
			},
		},
	}
}
