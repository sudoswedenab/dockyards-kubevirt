// Copyright 2026 Sudo Sweden AB
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
	"strings"

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	clusterGatewayParentRefNameKey      = "name"
	clusterGatewayParentRefNamespaceKey = "namespace"
)

func resolveClusterGatewayParentReference(
	ctx context.Context,
	c client.Client,
	ownerCluster *dockyardsv1.Cluster,
	fallback gatewayapiv1.ParentReference,
) (gatewayapiv1.ParentReference, error) {
	unstructuredDockyardsCluster := unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": dockyardsv1.GroupVersion.String(),
			"kind":       dockyardsv1.ClusterKind,
			"metadata": map[string]any{
				"name":      ownerCluster.Name,
				"namespace": ownerCluster.Namespace,
			},
		},
	}

	err := c.Get(ctx, client.ObjectKeyFromObject(&unstructuredDockyardsCluster), &unstructuredDockyardsCluster)
	if err != nil {
		return gatewayapiv1.ParentReference{}, err
	}

	clusterParentRef, found, err := clusterGatewayParentReference(unstructuredDockyardsCluster.Object)
	if err != nil {
		return gatewayapiv1.ParentReference{}, err
	}

	if found {
		return clusterParentRef, nil
	}

	return fallback, nil
}

func clusterGatewayParentReference(cluster map[string]any) (gatewayapiv1.ParentReference, bool, error) {
	name, found, err := unstructured.NestedString(cluster, "spec", "advanced", "gateway", "parentRef", clusterGatewayParentRefNameKey)
	if err != nil {
		return gatewayapiv1.ParentReference{}, false, err
	}

	name = strings.TrimSpace(name)
	if !found || name == "" {
		return gatewayapiv1.ParentReference{}, false, nil
	}

	namespace, found, err := unstructured.NestedString(cluster, "spec", "advanced", "gateway", "parentRef", clusterGatewayParentRefNamespaceKey)
	if err != nil {
		return gatewayapiv1.ParentReference{}, false, err
	}

	namespace = strings.TrimSpace(namespace)
	if !found || namespace == "" {
		return gatewayapiv1.ParentReference{}, false, nil
	}

	return gatewayapiv1.ParentReference{
		Name:      gatewayapiv1.ObjectName(name),
		Namespace: ptr.To(gatewayapiv1.Namespace(namespace)),
	}, true, nil
}
