# Changelog

## [0.8.0](https://github.com/briferz/crossplane-mcp/compare/v0.7.0...v0.8.0) (2026-07-25)


### Features

* **xp:** assess per-kind readiness of natively composed resources ([#53](https://github.com/briferz/crossplane-mcp/issues/53)) ([366bef8](https://github.com/briferz/crossplane-mcp/commit/366bef865cdca038c6c2975b1d1cceee5597f685))

## [0.7.0](https://github.com/briferz/crossplane-mcp/compare/v0.6.0...v0.7.0) (2026-07-25)


### Features

* **k8s:** invalidate stale discovery, bound requests, cover Get's scope branch ([#49](https://github.com/briferz/crossplane-mcp/issues/49)) ([8de4f5d](https://github.com/briferz/crossplane-mcp/commit/8de4f5d2eeb30c6f5672a3385d1350c3e911680a))
* **xp:** surface signals that drop at output boundaries ([#48](https://github.com/briferz/crossplane-mcp/issues/48)) ([02ae034](https://github.com/briferz/crossplane-mcp/commit/02ae0348c5e27e23d8fd4ed670864d6010bef91c))


### Bug Fixes

* **deps:** resolve reachable x/text CVE and refresh dependencies ([#43](https://github.com/briferz/crossplane-mcp/issues/43)) ([c577985](https://github.com/briferz/crossplane-mcp/commit/c5779853669e2c8ae02ff2ffaacd9794c9f0936b))
* **k8s:** project listed objects to triage fields in ListAll ([#50](https://github.com/briferz/crossplane-mcp/issues/50)) ([bc05b3e](https://github.com/briferz/crossplane-mcp/commit/bc05b3e193ffae86d35d14afac3e15e7ed44ce53))
* **record:** mask the value sibling when name/key names a secret ([#51](https://github.com/briferz/crossplane-mcp/issues/51)) ([d88279b](https://github.com/briferz/crossplane-mcp/commit/d88279b8be7ec08539aada247fee0e4ff9fe8bfc))
* **safety:** enforce the read-only and no-Secret-contents promises ([#47](https://github.com/briferz/crossplane-mcp/issues/47)) ([7654959](https://github.com/briferz/crossplane-mcp/commit/765495946c437afde7fa8f6c12a1ea8db767a070))
* **xp:** classify vocabulary-less native resources as Unknown ([#46](https://github.com/briferz/crossplane-mcp/issues/46)) ([fed2a8f](https://github.com/briferz/crossplane-mcp/commit/fed2a8f6aea3e7f3bbf78977a3d8c7a37de6ae1b))

## [0.6.0](https://github.com/briferz/crossplane-mcp/compare/v0.5.0...v0.6.0) (2026-06-12)


### Features

* add list_providers/list_functions/list_configurations package-health tools ([#38](https://github.com/briferz/crossplane-mcp/issues/38)) ([58b5918](https://github.com/briferz/crossplane-mcp/commit/58b591838060f81e6e679caa35cd351981796dd5))

## [0.5.0](https://github.com/briferz/crossplane-mcp/compare/v0.4.0...v0.5.0) (2026-06-10)


### Features

* read-only MCP annotations, lenient kinds, paused/finalizer signals, RBAC manifest ([#36](https://github.com/briferz/crossplane-mcp/issues/36)) ([fc2f854](https://github.com/briferz/crossplane-mcp/commit/fc2f85430cddeeac3374fde0242b8b709b7f4d17))

## [0.4.0](https://github.com/briferz/crossplane-mcp/compare/v0.3.0...v0.4.0) (2026-06-05)


### Features

* create the --log-file parent directory ([#34](https://github.com/briferz/crossplane-mcp/issues/34)) ([e55d6f2](https://github.com/briferz/crossplane-mcp/commit/e55d6f2663fae37b8b25c8eea4cb1ec4a62c9744))
* decode provider-terraform/OpenTofu base64+gzip error blobs in diagnose output ([#30](https://github.com/briferz/crossplane-mcp/issues/30)) ([2f29d93](https://github.com/briferz/crossplane-mcp/commit/2f29d9308c6c4a4473c77a244a850b0f0d8d7ba1))
* label suspect lifecycle (Terminating-stuck vs Creating-blocked) ([#33](https://github.com/briferz/crossplane-mcp/issues/33)) ([48ec24e](https://github.com/briferz/crossplane-mcp/commit/48ec24e4406d3f8766c67d4466b935351b4187ce))
* scrub high-precision secrets from --log-file records ([#32](https://github.com/briferz/crossplane-mcp/issues/32)) ([04ab335](https://github.com/briferz/crossplane-mcp/commit/04ab335a8adaccb67cc83214df2dc19437cc30f8))

## [0.3.0](https://github.com/briferz/crossplane-mcp/compare/v0.2.1...v0.3.0) (2026-06-04)


### Features

* add list_unhealthy tool to triage broken XRs and claims cluster-wide ([#28](https://github.com/briferz/crossplane-mcp/issues/28)) ([d36f1d9](https://github.com/briferz/crossplane-mcp/commit/d36f1d99fd623d75963764eb9f8db90e543e273d))
* weight recurring composition events over transport-flake conditions in diagnose ([#25](https://github.com/briferz/crossplane-mcp/issues/25)) ([27fac1a](https://github.com/briferz/crossplane-mcp/commit/27fac1ab0521fba61c756ec7f99f859767879630))

## [0.2.1](https://github.com/briferz/crossplane-mcp/compare/v0.2.0...v0.2.1) (2026-06-04)


### Bug Fixes

* bump Go to 1.26.4 to patch stdlib vulnerabilities (GO-2026-5037/5038/5039) ([#26](https://github.com/briferz/crossplane-mcp/issues/26)) ([4ab8de0](https://github.com/briferz/crossplane-mcp/commit/4ab8de0043a829d07b1e3ad659de75483d6603f5))

## [0.2.0](https://github.com/briferz/crossplane-mcp/compare/v0.1.1...v0.2.0) (2026-05-29)


### Features

* optional JSONL logging of tool calls (--log-file / CROSSPLANE_MCP_LOG_FILE) ([#21](https://github.com/briferz/crossplane-mcp/issues/21)) ([e85820e](https://github.com/briferz/crossplane-mcp/commit/e85820e0edbc008bac9bb660256cebea4f341c97))

## [0.1.1](https://github.com/briferz/crossplane-mcp/compare/v0.1.0...v0.1.1) (2026-05-29)


### Bug Fixes

* walk v2 namespaced XR composed refs at spec.crossplane.resourceRefs ([#18](https://github.com/briferz/crossplane-mcp/issues/18)) ([3a86fec](https://github.com/briferz/crossplane-mcp/commit/3a86fec9e429bfde8b16ab971e65ab61b3fbf319))

## [0.1.0](https://github.com/briferz/crossplane-mcp/compare/v0.0.1...v0.1.0) (2026-05-29)


### Features

* read-only Crossplane diagnostic MCP server (Phase 1 MVP) ([#1](https://github.com/briferz/crossplane-mcp/issues/1)) ([b9d85ce](https://github.com/briferz/crossplane-mcp/commit/b9d85ce1f6a98d2533a01ce891c6b9e45c7a7cc0))
