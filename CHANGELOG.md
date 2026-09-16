# Changelog

## [0.2.2](https://github.com/sudoswedenab/dockyards-kubevirt/compare/v0.2.1...v0.2.2) (2026-09-16)


### Bug Fixes

* reconciliation of NodePool to fail if NodeClass is not found ([338cb4a](https://github.com/sudoswedenab/dockyards-kubevirt/commit/338cb4a7073bfb8de7cf54bed8e5a4c1a224b866))

## [0.2.1](https://github.com/sudoswedenab/dockyards-kubevirt/compare/v0.2.0...v0.2.1) (2026-09-15)


### Features

* wire node preferences from NodeClass into KubevirtMachineTemplate ([6a3a580](https://github.com/sudoswedenab/dockyards-kubevirt/commit/6a3a5806f4c9751b51ba0657bce3b5f4ff4eeaf9))


### Bug Fixes

* expect NodeClass to live in the publicNamespace ([3e47ec6](https://github.com/sudoswedenab/dockyards-kubevirt/commit/3e47ec650cb095ddb3f1e1b74492e7643dc34bdd))
* kubebuilder tags for nodeclasses lookups ([259a9cb](https://github.com/sudoswedenab/dockyards-kubevirt/commit/259a9cb6e935efc1ddcc3090df9645eeede01cee))

## [0.2.0](https://github.com/sudoswedenab/dockyards-kubevirt/compare/v0.1.0...v0.2.0) (2026-09-15)


### ⚠ BREAKING CHANGES

* sidero-community compatabilty change

### Features

* ensure that both KubevirtCluster and TalosControlPlane have DY labels ([ad01593](https://github.com/sudoswedenab/dockyards-kubevirt/commit/ad015931a6018f1b0f6e657af93e00ace279a8c6))
* kubevirt cluster to be created based on Dockyards cluster ([7ac0976](https://github.com/sudoswedenab/dockyards-kubevirt/commit/7ac09762340797f6bf5cbd438300c92fbff3c55b))
* remove the logic for linking taloscontrolplane to CAPI Cluster spec ([b6f2422](https://github.com/sudoswedenab/dockyards-kubevirt/commit/b6f24228f3655cfab5883b7a0e5629054662a09d))


### Bug Fixes

* ensure that APIGroup receives group-only imput ([ed14ecb](https://github.com/sudoswedenab/dockyards-kubevirt/commit/ed14ecbfc7051d35f89f1b96e86d67c0924d8e4f))


### Code Refactoring

* sidero-community compatabilty change ([94cd55c](https://github.com/sudoswedenab/dockyards-kubevirt/commit/94cd55c6a50579aa1321bff6adbe52f740b67169))

## 0.1.0

- Historical release baseline before adopting release-please.
