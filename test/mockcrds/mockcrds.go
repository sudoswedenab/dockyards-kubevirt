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

package mockcrds

import (
	networkv1 "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	bootstrapv1 "github.com/siderolabs/cluster-api-bootstrap-provider-talos/api/v1alpha3"
	controlplanev1 "github.com/siderolabs/cluster-api-control-plane-provider-talos/api/v1alpha3"
	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	providerv1 "sigs.k8s.io/cluster-api-provider-kubevirt/api/v1alpha1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

var (
	DockyardsNodePool       = mockCRD(dockyardsv1.NodePoolKind, "nodepools", dockyardsv1.GroupVersion.Group, dockyardsv1.GroupVersion.Version)
	DockyardsRelease        = mockCRD(dockyardsv1.ReleaseKind, "releases", dockyardsv1.GroupVersion.Group, dockyardsv1.GroupVersion.Version)
	KubevirtMachineTemplate = mockCRD("KubevirtMachineTemplate", "kubevirtmachinetemplates", providerv1.GroupVersion.Group, providerv1.GroupVersion.Version)
	CDIDataVolume           = mockCRD("DataVolume", "datavolumes", cdiv1.CDIGroupVersionKind.Group, cdiv1.CDIGroupVersionKind.Version)
	CDIDataSource           = mockCRD("DataSource", "datasources", cdiv1.CDIGroupVersionKind.Group, cdiv1.CDIGroupVersionKind.Version)
	DockyardsCluster        = mockCRD(dockyardsv1.ClusterKind, "clusters", dockyardsv1.GroupVersion.Group, dockyardsv1.GroupVersion.Version)
	TalosControlPlane       = mockCRD("TalosControlPlane", "taloscontrolplanes", controlplanev1.GroupVersion.Group, controlplanev1.GroupVersion.Version)
	CAPICluster             = mockCRD(clusterv1.ClusterKind, "clusters", clusterv1.GroupVersion.Group, clusterv1.GroupVersion.Version)
	TalosConfigTemplate     = mockCRD("TalosConfigTemplate", "talosconfigtemplates", bootstrapv1.GroupVersion.Group, bootstrapv1.GroupVersion.Version)
	DockyardsWorkload       = mockCRD(dockyardsv1.WorkloadKind, "workloads", dockyardsv1.GroupVersion.Group, dockyardsv1.GroupVersion.Version)

	CRDs = []*apiextensionsv1.CustomResourceDefinition{
		DockyardsNodePool,
		DockyardsRelease,
		KubevirtMachineTemplate,
		CDIDataVolume,
		CDIDataSource,
		DockyardsCluster,
		TalosControlPlane,
		CAPICluster,
		TalosConfigTemplate,
		DockyardsWorkload,
	}

	NetworkAttachmentDefinition = mockCRD("NetworkAttachmentDefinition", "network-attachment-definitions", networkv1.SchemeGroupVersion.Group, networkv1.SchemeGroupVersion.Version)

	MultusCRDs = []*apiextensionsv1.CustomResourceDefinition{
		NetworkAttachmentDefinition,
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
