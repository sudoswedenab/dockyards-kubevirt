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
	"strings"

	dockyardsv1 "github.com/sudoswedenab/dockyards-backend/api/v1alpha3"
	"k8s.io/utils/ptr"
	gatewayapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func resolveClusterGatewayParentReference(
	ownerCluster *dockyardsv1.Cluster,
	fallback gatewayapiv1.ParentReference,
) (gatewayapiv1.ParentReference, error) {
	clusterParentRef, found, err := clusterGatewayParentReference(ownerCluster)
	if err != nil {
		return gatewayapiv1.ParentReference{}, err
	}

	if found {
		return clusterParentRef, nil
	}

	return fallback, nil
}

func clusterGatewayParentReference(cluster *dockyardsv1.Cluster) (gatewayapiv1.ParentReference, bool, error) {
	name := cluster.Spec.Advanced.Gateway.ParentRef.Name

	name = strings.TrimSpace(name)
	if name == "" {
		return gatewayapiv1.ParentReference{}, false, nil
	}

	namespace := cluster.Spec.Advanced.Gateway.ParentRef.Namespace

	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return gatewayapiv1.ParentReference{}, false, nil
	}

	return gatewayapiv1.ParentReference{
		Name:      gatewayapiv1.ObjectName(name),
		Namespace: ptr.To(gatewayapiv1.Namespace(namespace)),
	}, true, nil
}
