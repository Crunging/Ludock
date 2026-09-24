# Changelog

## [0.4.0](https://github.com/Crunging/Ludock/compare/v0.3.0...v0.4.0) (2026-09-24)


### ⚠ BREAKING CHANGES

* Password authentication accepts Argon2id hashes only; legacy scrypt hashes no longer authenticate.

### Bug Fixes

* **ci:** stage Release Please assets under Bun ([#21](https://github.com/Crunging/Ludock/issues/21)) ([07246ef](https://github.com/Crunging/Ludock/commit/07246eff2c785cbc39022046caf975e351756845))
* handle log redaction and recovery edge cases ([#25](https://github.com/Crunging/Ludock/issues/25)) ([1f58d21](https://github.com/Crunging/Ludock/commit/1f58d216a4cae3097aa277744db7ec60717c1a7c))
* harden runtimes without adding setup requirements ([#27](https://github.com/Crunging/Ludock/issues/27)) ([20aa79e](https://github.com/Crunging/Ludock/commit/20aa79e64c4ecbc595a04277c5dc3276798af4f0))


### Performance Improvements

* reduce database and stream processing overhead ([#23](https://github.com/Crunging/Ludock/issues/23)) ([d67b6aa](https://github.com/Crunging/Ludock/commit/d67b6aa111224f28961d6af0b5c025b8a5f47e82))


### Code Refactoring

* remove dead code, simplify dev tooling and CI, type-check backend tests ([#28](https://github.com/Crunging/Ludock/issues/28)) ([c4830a4](https://github.com/Crunging/Ludock/commit/c4830a4d86326f9b0bb2be04e88146b812c6989f))
* use native Bun I/O and Bun-only tooling ([#20](https://github.com/Crunging/Ludock/issues/20)) ([85b673f](https://github.com/Crunging/Ludock/commit/85b673f686770c175322c88592118ab11c55b1e9))

## [0.3.0](https://github.com/Crunging/Ludock/compare/v0.2.1...v0.3.0) (2026-09-15)


### Features

* add backup readiness and storage visibility ([#13](https://github.com/Crunging/Ludock/issues/13)) ([9c5ecc3](https://github.com/Crunging/Ludock/commit/9c5ecc333a75ca47fce6ba6a8bde93c08cfa669d))
* add dashboard needs-attention view ([#16](https://github.com/Crunging/Ludock/issues/16)) ([3e968c0](https://github.com/Crunging/Ludock/commit/3e968c0cf6aa4ef90ed432a119c5c5e54b379b59))
* add searchable audit and operation history ([#15](https://github.com/Crunging/Ludock/issues/15)) ([b85b436](https://github.com/Crunging/Ludock/commit/b85b43617cd061bb2e968d437a6cbd0a4c7d7d35))
* discover Compose sources without project registration ([7b8dc62](https://github.com/Crunging/Ludock/commit/7b8dc622863a5adff09576f5dcccbbc6cb692606))
* **notifications:** add Discord delivery troubleshooting ([#14](https://github.com/Crunging/Ludock/issues/14)) ([d2d288b](https://github.com/Crunging/Ludock/commit/d2d288b3e3ccf18775d55b25f6dafbd66bd911db))
* **schedules:** add editing, pause controls, and run visibility ([#9](https://github.com/Crunging/Ludock/issues/9)) ([5fe7170](https://github.com/Crunging/Ludock/commit/5fe7170ad709af7c1f1e0afff38539c0eb786d3c))
* **ui:** add log file and activity browsing filters ([#10](https://github.com/Crunging/Ludock/issues/10)) ([85b4c5b](https://github.com/Crunging/Ludock/commit/85b4c5b4e5019ab285180a88bb70a89eef1e3ead))


### Bug Fixes

* **backend:** skip unavailable Compose probes and isolate HTTP tests ([2b8fbdd](https://github.com/Crunging/Ludock/commit/2b8fbddc9b70c1818c49b6b72fec77e86dd286cc))
* **ci:** complete published release PR state ([1059599](https://github.com/Crunging/Ludock/commit/10595995963ba90a3488af287bd3c0d55901fe86))
* reliability fixes and Bun tooling ([#19](https://github.com/Crunging/Ludock/issues/19)) ([def1c55](https://github.com/Crunging/Ludock/commit/def1c55bbb286654d1260bd7a19ea11faf1f7fdd))
* simplify setup and in-app configuration ([#18](https://github.com/Crunging/Ludock/issues/18)) ([a88e0ac](https://github.com/Crunging/Ludock/commit/a88e0acb2fd39f9bb30122041675fb954b46c888))
* **ui:** align interactive controls with the orange brand accent ([#11](https://github.com/Crunging/Ludock/issues/11)) ([bc3853a](https://github.com/Crunging/Ludock/commit/bc3853afdf79cc2869f5e01474d76f240a7bba66))
* use official distroless helper and update supported dependencies ([#12](https://github.com/Crunging/Ludock/issues/12)) ([f1d4618](https://github.com/Crunging/Ludock/commit/f1d461869330bce96fd8c62d141083c218f9d6dc))


### Code Refactoring

* **frontend:** consolidate page routing and read lifetimes ([3466da0](https://github.com/Crunging/Ludock/commit/3466da03501cd0826aa03bf642e7f46c1c09f41c))
* simplify reads, helpers, and maintenance ([#17](https://github.com/Crunging/Ludock/issues/17)) ([7d84763](https://github.com/Crunging/Ludock/commit/7d8476379f17f965134fb5c1d5326e9777cbd055))
* simplify server workflows and reduce refresh overhead ([#7](https://github.com/Crunging/Ludock/issues/7)) ([f43633d](https://github.com/Crunging/Ludock/commit/f43633d74eef41ad3cc3152cc7549ae48330482b))

## [0.2.1](https://github.com/Crunging/Ludock/compare/v0.2.0...v0.2.1) (2026-09-10)


### Bug Fixes

* **security:** harden setup, durable authority, and runtime boundaries ([#5](https://github.com/Crunging/Ludock/issues/5)) ([973ea35](https://github.com/Crunging/Ludock/commit/973ea35293d9eb53ffc52d5b1b655b6200164e64))
