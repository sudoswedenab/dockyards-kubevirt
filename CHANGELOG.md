# Changelog

## [0.2.4](https://github.com/sudoswedenab/dockyards-kubevirt/compare/v0.2.3...v0.2.4) (2026-09-30)


### Bug Fixes

* ensure that we can watch and list dockyards virtualmachines ([bad498c](https://github.com/sudoswedenab/dockyards-kubevirt/commit/bad498ca5ae7a04a4dff06e9fce84184b671e50e))
* missing virtualmachineinstances/status rbac ([7baceca](https://github.com/sudoswedenab/dockyards-kubevirt/commit/7bacecab0ad7e5ee2d0e7518842770a1b7f842c6))
* stale use of name virtualmachine where virtualmachineinstances should have been used ([d2a6951](https://github.com/sudoswedenab/dockyards-kubevirt/commit/d2a695149c0e42b58806c366ecd6f94f09aa322a))
* update status field of virtualmachineinstance after creating it ([61e1b6d](https://github.com/sudoswedenab/dockyards-kubevirt/commit/61e1b6da284be563add5e1f378b57a9f6b61aad8))

## [0.2.3](https://github.com/sudoswedenab/dockyards-kubevirt/compare/v0.2.2...v0.2.3) (2026-09-29)


### Features

* proxy kubevirt virtualmachine status to dockyards virtualmachine ([9704490](https://github.com/sudoswedenab/dockyards-kubevirt/commit/970449072affdf212d3d0cb5383b8009c3b454e5))

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
