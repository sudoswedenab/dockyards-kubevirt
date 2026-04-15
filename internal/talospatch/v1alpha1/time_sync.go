package v1alpha1

import (
	"gopkg.in/yaml.v3"
)

const TimeSyncConfigAPIVersion = "v1alpha1"
const TimeSyncConfigKind = "TimeSyncConfig"

type TimeSyncConfig struct {
	Meta `yaml:",inline"`

	NTP NTPConfig `yaml:"ntp,omitempty"`
	PTP PTPConfig `yaml:"ptp,omitempty"`
}
var _ yaml.IsZeroer = &TimeSyncConfig{}

func (c *TimeSyncConfig) IsZero() bool {
	if !c.NTP.IsZero() {
		return false
	}
	if !c.PTP.IsZero() {
		return false
	}
	return true
}

type NTPConfig struct {
	Servers []string `yaml:"servers,omitempty"`
}
var _ yaml.IsZeroer = &NTPConfig{}

func (c *NTPConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if c.Servers != nil {
		return false
	}
	return true
}

type PTPConfig struct {
	Devices []string `yaml:"devices,omitempty"`
}
var _ yaml.IsZeroer = &PTPConfig{}

func (c *PTPConfig) IsZero() bool {
	// nolint:staticcheck // This struct will probably be extended in the future
	if c.Devices != nil {
		return false
	}
	return true
}
