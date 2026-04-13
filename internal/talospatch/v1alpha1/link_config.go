package v1alpha1

import "gopkg.in/yaml.v3"

// LinkConfig is a Talos config document used to configure a network link.
// We only model the bits we need (routes) for strategic patching.
const LinkConfigAPIVersion = "v1alpha1"
const LinkConfigKind = "LinkConfig"

type LinkConfig struct {
	Meta `yaml:",inline"`

	Name   string        `yaml:"name,omitempty"`
	Routes []RouteConfig `yaml:"routes,omitempty"`
}

var _ yaml.IsZeroer = &LinkConfig{}

func (c *LinkConfig) IsZero() bool {
	// We treat the document as empty unless it configures something.
	return len(c.Routes) == 0
}

// RouteConfig matches Talos RouteConfig fields:
// https://docs.siderolabs.com/talos/v1.12/reference/configuration/network/linkconfig#routes
type RouteConfig struct {
	Destination string `yaml:"destination,omitempty"`
	Gateway     string `yaml:"gateway,omitempty"`
	Source      string `yaml:"source,omitempty"`

	Metric *uint32 `yaml:"metric,omitempty"`
	MTU    *uint32 `yaml:"mtu,omitempty"`

	// Table is intentionally loosely typed to allow Talos' RoutingTable values.
	Table any `yaml:"table,omitempty"`
}
