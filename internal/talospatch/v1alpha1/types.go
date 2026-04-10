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

package v1alpha1

import (
	"os"

	"gopkg.in/yaml.v3"
)

type ConfigVersionType string
const ConfigVersion ConfigVersionType = "v1alpha1"
type Config struct {
	Version ConfigVersionType `yaml:"version"`
	Machine MachineConfig     `yaml:"machine,omitempty"`
	Cluster ClusterConfig     `yaml:"cluster,omitempty"`
}
var _ yaml.IsZeroer = &Config{}

func (c *Config) IsZero() bool {
	if !c.Machine.IsZero() {
		return false
	}
	if !c.Cluster.IsZero() {
		return false
	}
	return true
}

type MachineConfig struct {
	Kubelet KubeletConfig `yaml:"kubelet,omitempty"`
	Env     Env           `yaml:"env,omitempty"`
	Files   []MachineFile `yaml:"files,omitempty"`
}
var _ yaml.IsZeroer = &MachineConfig{}

func (c *MachineConfig) IsZero() bool {
	if !c.Kubelet.IsZero() {
		return false
	}
	if !c.Env.IsZero() {
		return false
	}
	if c.Files != nil {
		return false
	}
	return true
}

type KubeletConfig struct {
	NodeIP KubeletNodeIPConfig `yaml:"nodeIP,omitempty"`
}
var _ yaml.IsZeroer = &KubeletConfig{}

func (k *KubeletConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if !k.NodeIP.IsZero() {
		return false
	}
	return true
}

type Env map[string]string
var _ yaml.IsZeroer = &Env{}

func (e *Env) IsZero() bool {
	return *e == nil
}

func (e *Env) Set(key string, value string) *Env {
	if *e == nil {
		*e = map[string]string{}
	}
	(*e)[key] = value
	return e
}

type MachineFile struct {
	Content string `yaml:"content,omitempty"`
	Permissions os.FileMode `yaml:"permissions,omitempty"`
	Path string `yaml:"path,omitempty"`
	Op string `yaml:"op,omitempty"`
}
var _ yaml.IsZeroer = &MachineFile{}

func (f *MachineFile) IsZero() bool {
	if f.Content != "" {
		return false
	}
	if f.Permissions != 0 {
		return false
	}
	if f.Path != "" {
		return false
	}
	if f.Op!= "" {
		return false
	}
	return true
}


type KubeletNodeIPConfig struct {
	ValidSubnets []string `yaml:"validSubnets,omitempty"`
}
var _ yaml.IsZeroer = &KubeletNodeIPConfig{}

func (c *KubeletNodeIPConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if c.ValidSubnets != nil {
		return false
	}
	return true
}

type ClusterNetworkConfig struct {
	PodSubnets     []string  `yaml:"podSubnets,omitempty" merge:"replace"`
	ServiceSubnets []string  `yaml:"serviceSubnets,omitempty" merge:"replace"`
	CNI            CNIConfig `yaml:"cni,omitempty"`
}
var _ yaml.IsZeroer = &ClusterNetworkConfig{}

func (c *ClusterNetworkConfig) IsZero() bool {
	if c.PodSubnets != nil {
		return false
	}
	if c.ServiceSubnets != nil {
		return false
	}
	if !c.CNI.IsZero() {
		return false
	}
	return true
}

type CNIConfig struct {
	Name *string `yaml:"name,omitempty"`
}
var _ yaml.IsZeroer = &CNIConfig{}

func (c *CNIConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if c.Name != nil {
		return false
	}
	return true
}

type ETCDConfig struct {
	AdvertisedSubnets []string `yaml:"advertisedSubnets,omitempty"`
	ListenSubnets     []string `yaml:"listenSubnets,omitempty"`
}
var _ yaml.IsZeroer = &ETCDConfig{}

func (c *ETCDConfig) IsZero() bool {
	if c.AdvertisedSubnets != nil {
		return false
	}
	if c.ListenSubnets != nil {
		return false
	}
	return true
}

type ClusterConfig struct {
	Network   ClusterNetworkConfig   `yaml:"network,omitempty"`
	APIServer APIServerConfig        `yaml:"apiServer,omitempty"`
	ETCD      ETCDConfig             `yaml:"etcd,omitempty"`
	Discovery ClusterDiscoveryConfig `yaml:"discovery,omitempty"`
}
var _ yaml.IsZeroer = &ClusterConfig{}

func (c *ClusterConfig) IsZero() bool {
	if !c.Network.IsZero() {
		return false
	}
	if !c.APIServer.IsZero() {
		return false
	}
	if !c.ETCD.IsZero() {
		return false
	}
	if !c.Discovery.IsZero() {
		return false
	}
	return true
}

type APIServerConfig struct {
	CertSANs  []string `yaml:"certSANs,omitempty"`
	ExtraArgs Args    `yaml:"extraArgs,omitempty"`
}
var _ yaml.IsZeroer = &APIServerConfig{}

func (c *APIServerConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if c.CertSANs != nil {
		return false
	}
	return true
}

type Args map[string][]string
var _ yaml.IsZeroer = &Args{}

func (a *Args) IsZero() bool {
	return *a == nil
}

func (a *Args) Add(key string, value string) {
	if *a == nil {
		*a = map[string][]string{}
	}
	(*a)[key] = append((*a)[key], value)
}

type ClusterDiscoveryConfig struct {
	Registries DiscoveryRegistriesConfig `yaml:"registries,omitempty"`
}
var _ yaml.IsZeroer = &ClusterDiscoveryConfig{}

func (c *ClusterDiscoveryConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if !c.Registries.IsZero() {
		return false
	}
	return true
}

type DiscoveryRegistriesConfig struct {
	Service RegistryServiceConfig `yaml:"service,omitempty"`
}
var _ yaml.IsZeroer = &DiscoveryRegistriesConfig{}

func (c *DiscoveryRegistriesConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if !c.Service.IsZero() {
		return false
	}
	return true
}

type RegistryServiceConfig struct {
	Disabled *bool  `yaml:"disabled,omitempty"`
	Endpoint string `yaml:"endpoint,omitempty"`
}
var _ yaml.IsZeroer = &RegistryServiceConfig{}

func (c *RegistryServiceConfig) IsZero() bool {
	if c.Disabled != nil {
		return false
	}
	if c.Endpoint != "" {
		return false
	}
	return true
}
