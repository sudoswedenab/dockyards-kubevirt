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

package controllers

// talosV1Alpha1ConfigPatch is a minimal v1alpha1 Talos machine config document
// used as a strategic merge patch.
//
// We keep this intentionally small (and use lots of `omitempty`) to avoid
// accidentally zeroing fields we don't manage.
type talosV1Alpha1ConfigPatch struct {
	Version string                     `yaml:"version"`
	Machine *talosV1Alpha1MachinePatch `yaml:"machine,omitempty"`
	Cluster *talosV1Alpha1ClusterPatch `yaml:"cluster,omitempty"`
}

type talosV1Alpha1MachinePatch struct {
	Kubelet *talosV1Alpha1KubeletPatch `yaml:"kubelet,omitempty"`
	Env     *talosV1Alpha1EnvPatch     `yaml:"env,omitempty"`
}

type talosV1Alpha1EnvPatch struct {
	HTTPProxy  *string `yaml:"http_proxy,omitempty"`
	HTTPSProxy *string `yaml:"https_proxy,omitempty"`
	NoProxy    *string `yaml:"no_proxy,omitempty"`
}

type talosV1Alpha1KubeletPatch struct {
	NodeIP *talosV1Alpha1KubeletNodeIPPatch `yaml:"nodeIP,omitempty"`
}

type talosV1Alpha1KubeletNodeIPPatch struct {
	ValidSubnets []string `yaml:"validSubnets,omitempty"`
}

type talosV1Alpha1ClusterPatch struct {
	Network   *talosV1Alpha1ClusterNetworkPatch `yaml:"network,omitempty"`
	APIServer *talosV1Alpha1APIServerPatch      `yaml:"apiServer,omitempty"`
	ETCD      *talosV1Alpha1ETCDPatch           `yaml:"etcd,omitempty"`
}

type talosV1Alpha1APIServerPatch struct {
	CertSANs []string `yaml:"certSANs,omitempty"`
}

type talosV1Alpha1ClusterNetworkPatch struct {
	PodSubnets     []string                      `yaml:"podSubnets,omitempty"`
	ServiceSubnets []string                      `yaml:"serviceSubnets,omitempty"`
	CNI            *talosV1Alpha1ClusterCNIPatch `yaml:"cni,omitempty"`
}

type talosV1Alpha1ETCDPatch struct {
	AdvertisedSubnets []string `yaml:"advertisedSubnets,omitempty"`
	ListenSubnets     []string `yaml:"listenSubnets,omitempty"`
}

type talosV1Alpha1ClusterCNIPatch struct {
	Name string `yaml:"name,omitempty"`
}

type timeSyncConfigNTP struct {
	Servers []string `yaml:"servers,omitempty"`
}

type timeSyncConfigDoc struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	NTP        *timeSyncConfigNTP `yaml:"ntp,omitempty"`
}
