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

import (
	dyconfig "github.com/sudoswedenab/dockyards-backend/api/config"
)

// TODO: consider smarter loading of vars based on app prefix
const (
	KeyNoProxy          dyconfig.Key = "dockyards-kubevirt.env.NO_PROXY"
	KeyHttpProxy        dyconfig.Key = "dockyards-kubevirt.env.HTTP_PROXY"
	KeyHttpsProxy       dyconfig.Key = "dockyards-kubevirt.env.HTTPS_PROXY"
	KeyNtpServers       dyconfig.Key = "dockyards-kubevirt.ntp.servers"
	KeyPtpDevices       dyconfig.Key = "dockyards-kubevirt.ptp.devices"
)
