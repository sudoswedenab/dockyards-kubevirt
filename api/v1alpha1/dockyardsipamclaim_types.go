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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DockyardsIPAMClaimSpec defines desired state of DockyardsIPAMClaim.
type DockyardsIPAMClaimSpec struct {
	ClusterName  string `json:"clusterName"`
	MachineName  string `json:"machineName"`
	Interface    string `json:"interface"`
	Subnet       string `json:"subnet"`
	ControlPlane bool   `json:"controlPlane"`
	Address      string `json:"address,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=dockyardsipamclaims,scope=Namespaced,categories=dockyards

// DockyardsIPAMClaim is the schema for Dockyards IPAM claims.
type DockyardsIPAMClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DockyardsIPAMClaimSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// DockyardsIPAMClaimList contains a list of DockyardsIPAMClaim.
type DockyardsIPAMClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DockyardsIPAMClaim `json:"items"`
}

func (in *DockyardsIPAMClaim) DeepCopyInto(out *DockyardsIPAMClaim) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
}

func (in *DockyardsIPAMClaim) DeepCopy() *DockyardsIPAMClaim {
	if in == nil {
		return nil
	}

	out := new(DockyardsIPAMClaim)
	in.DeepCopyInto(out)

	return out
}

func (in *DockyardsIPAMClaim) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}

	return nil
}

func (in *DockyardsIPAMClaimList) DeepCopyInto(out *DockyardsIPAMClaimList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		in, out := &in.Items, &out.Items
		*out = make([]DockyardsIPAMClaim, len(*in))
		for i := range *in {
			(*in)[i].DeepCopyInto(&(*out)[i])
		}
	}
}

func (in *DockyardsIPAMClaimList) DeepCopy() *DockyardsIPAMClaimList {
	if in == nil {
		return nil
	}

	out := new(DockyardsIPAMClaimList)
	in.DeepCopyInto(out)

	return out
}

func (in *DockyardsIPAMClaimList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}

	return nil
}
