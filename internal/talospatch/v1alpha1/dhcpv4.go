package v1alpha1

import "gopkg.in/yaml.v3"

const DHCPv4ConfigAPIVersion = "v1alpha1"
const DHCPv4ConfigKind = "DHCPv4Config"

// DHCPv4Config is a Talos config document used to enable/configure DHCPv4.
// We only model the bits we need (name) for strategic patching.
type DHCPv4Config struct {
	Meta `yaml:",inline"`

	Name string `yaml:"name,omitempty"`
}

var _ yaml.IsZeroer = &DHCPv4Config{}

func (c *DHCPv4Config) IsZero() bool {
	// We treat the document as empty unless it configures something.
	return c.Name == ""
}
