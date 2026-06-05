# Changelog

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
