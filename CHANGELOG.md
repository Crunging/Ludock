# Changelog

## [0.6.0](https://github.com/Crunging/Ludock/compare/v0.5.0...v0.6.0) (2026-10-29)


### ⚠ BREAKING CHANGES

* Ludock no longer opens databases from 0.5 and earlier. Start with a new application data volume (or LUDOCK_DATA_DIR) and complete first-administrator setup again; the old database and game data are left unchanged. The JSON API and LUDOCK_API_TOKEN are removed (only /api/v1/health remains), as is AUDIT_LOG_MAX_ROWS. Renamed settings: MAX_UPLOAD_SIZE is LUDOCK_MAX_UPLOAD_SIZE, LOG_LEVEL is LUDOCK_LOG_LEVEL, DOCKER_SOCKET is LUDOCK_DOCKER_SOCKET, FILE_HELPER_IMAGE is LUDOCK_HELPER_IMAGE, and LUDOCK_DB_PATH is replaced by LUDOCK_DATA_DIR.

### Features

* record and graph each server's CPU and memory history ([#64](https://github.com/Crunging/Ludock/issues/64)) ([100bc10](https://github.com/Crunging/Ludock/commit/100bc10affaabb370669b5c1fc18efffb5078a20))
* rewrite Ludock in Go ([#50](https://github.com/Crunging/Ludock/issues/50)) ([a600acd](https://github.com/Crunging/Ludock/commit/a600acd7a22fe6f84e6ef5648aa8bce9a4d80814))
* send alerts to ntfy, Gotify, Telegram, email, or any webhook ([#66](https://github.com/Crunging/Ludock/issues/66)) ([da3a427](https://github.com/Crunging/Ludock/commit/da3a427aee7c6402323dfafe40447dc0ce7bf597))
* show who's online on each Minecraft server ([#65](https://github.com/Crunging/Ludock/issues/65)) ([0bbc634](https://github.com/Crunging/Ludock/commit/0bbc634559eb6fbccb6318d7a1d3227711df0906))
* smooth out the server list, server pages, and phone layout ([#58](https://github.com/Crunging/Ludock/issues/58)) ([f11169e](https://github.com/Crunging/Ludock/commit/f11169e070038d29230b07aafa17063b0d6a1c4f))
* update servers through the Docker API without Compose mounts ([#57](https://github.com/Crunging/Ludock/issues/57)) ([1891590](https://github.com/Crunging/Ludock/commit/1891590104085b5c5f2e2c55382593b843c9aa1e))


### Bug Fixes

* **console:** finish commands sent before the browser leaves ([#71](https://github.com/Crunging/Ludock/issues/71)) ([6b7d788](https://github.com/Crunging/Ludock/commit/6b7d7888ffe5126c35e1391fe0f812a41528d74c))
* hide more container credentials from logs and consoles ([#60](https://github.com/Crunging/Ludock/issues/60)) ([d937b25](https://github.com/Crunging/Ludock/commit/d937b25dab085f15aeb12611ce9f7b7856d8b75e))
* keep sign-in redirects on site when next contains a tab ([#54](https://github.com/Crunging/Ludock/issues/54)) ([da72079](https://github.com/Crunging/Ludock/commit/da7207919ca83f5ad6837c722dd1b3458508505c))
* **ops:** never run a job twice when it finishes while the queue is read ([#75](https://github.com/Crunging/Ludock/issues/75)) ([1f4e670](https://github.com/Crunging/Ludock/commit/1f4e67014653acfd68d0bf333bce33e2a972c37c))
* **players:** read players again when a command lands during a reading ([#74](https://github.com/Crunging/Ludock/issues/74)) ([02b4ed0](https://github.com/Crunging/Ludock/commit/02b4ed099bdcef22fc67dd2a2f20dd2d24c407f7))
* require Go 1.27.3 to patch HTTP/2 and certificate vulnerabilities ([#63](https://github.com/Crunging/Ludock/issues/63)) ([6ccdf22](https://github.com/Crunging/Ludock/commit/6ccdf22a8b0da4cfd8f7c3dc14d7ee0fee046cc3))
* show the setup guidance Ludock already writes, and add staticcheck ([#73](https://github.com/Crunging/Ludock/issues/73)) ([e64bfda](https://github.com/Crunging/Ludock/commit/e64bfdae554c7b32a319073481ff53c197052f90))
* show the whole More menu on the last server in the list ([#53](https://github.com/Crunging/Ludock/issues/53)) ([2f636d8](https://github.com/Crunging/Ludock/commit/2f636d80617d6055331faba2d54ee467e6cd114c))


### Performance Improvements

* cache SQLite statements and compress pages ([#59](https://github.com/Crunging/Ludock/issues/59)) ([08570f4](https://github.com/Crunging/Ludock/commit/08570f4cd848e778e16a477cc6b1a2cc8ac2ecdd))
* reduce repeated Docker, database, and browser work ([#52](https://github.com/Crunging/Ludock/issues/52)) ([8f3d9ad](https://github.com/Crunging/Ludock/commit/8f3d9ad59e614a78c3deba0874b9dbc7904edb83))
* send unchanged live refreshes as 304s, shrink fonts by a third, and say when Ludock can't be reached ([#68](https://github.com/Crunging/Ludock/issues/68)) ([950d0e4](https://github.com/Crunging/Ludock/commit/950d0e4ebad949e96d1dcaa31d990537f1eabe91))


### Code Refactoring

* check the second character of next explicitly in safeNext ([#55](https://github.com/Crunging/Ludock/issues/55)) ([cac3fed](https://github.com/Crunging/Ludock/commit/cac3fede3c4b3f0c0e05f76348e1205bd755933a))
* **console:** send Minecraft commands over RCON from Ludock ([#62](https://github.com/Crunging/Ludock/issues/62)) ([817ef79](https://github.com/Crunging/Ludock/commit/817ef79ff69a7655ade1bb1fa10e5746f6dccd28))
* cut comments that restate the code so agents read less noise ([#72](https://github.com/Crunging/Ludock/issues/72)) ([ecef458](https://github.com/Crunging/Ludock/commit/ecef458b43cea32e1f6a50d1f9a69b27896050f4))
* drop the timing settings added for CI ([01a21f7](https://github.com/Crunging/Ludock/commit/01a21f783fe60c701b0b603a3db9edaad0a84a14))
* **helper:** write archive headers with the shared tar encoder ([#61](https://github.com/Crunging/Ludock/issues/61)) ([9de64b6](https://github.com/Crunging/Ludock/commit/9de64b6ae5ad8f8e4ba99e62ef8e392fa566612a))
* **jobs:** give operation recovery state a typed struct agents can navigate ([#76](https://github.com/Crunging/Ludock/issues/76)) ([a2cae12](https://github.com/Crunging/Ludock/commit/a2cae12318d7c255b15097ce6cfb84c6aa30e8cf))
* split backup and restore code into their own files ([#77](https://github.com/Crunging/Ludock/issues/77)) ([c468e47](https://github.com/Crunging/Ludock/commit/c468e47abf941bfccf6662b414e7c4119aab9af6))
* use one mount type for servers, files, and helpers ([#78](https://github.com/Crunging/Ludock/issues/78)) ([4a0e24b](https://github.com/Crunging/Ludock/commit/4a0e24b797e7ca7aa7e845723865f96cf63eb6a0))

## [0.5.0](https://github.com/Crunging/Ludock/compare/v0.4.0...v0.5.0) (2026-10-18)


### Features

* **schedules:** one-time scheduled actions ([#45](https://github.com/Crunging/Ludock/issues/45)) ([7e34307](https://github.com/Crunging/Ludock/commit/7e3430713aa44d150ca65a732192b7f4c8b67ea1))
* **schedules:** restart countdown announcements ([#46](https://github.com/Crunging/Ludock/issues/46)) ([38a6fae](https://github.com/Crunging/Ludock/commit/38a6fae3908c08ddd1b77a90f16d2fb92cd27095))
* support backups before scheduled restarts ([#44](https://github.com/Crunging/Ludock/issues/44)) ([8e3ecf8](https://github.com/Crunging/Ludock/commit/8e3ecf816a829d4be7d7a9acc4a673fd78cddfcc))


### Bug Fixes

* **compose:** apply YAML merge keys and reject !reset and !override tags ([63bbdc0](https://github.com/Crunging/Ludock/commit/63bbdc0428df14d89b0747a2b24abcea9a9df03f))
* **schedules:** reset kept draft values and free lanes during warning cleanup ([#47](https://github.com/Crunging/Ludock/issues/47)) ([e498121](https://github.com/Crunging/Ludock/commit/e498121aa59f20a4972cc49ff8a2800472174400))
* warn instead of failing on group or ACL access to the identity key ([#49](https://github.com/Crunging/Ludock/issues/49)) ([0de5089](https://github.com/Crunging/Ludock/commit/0de50898a6b337a540bbfc2fd3078782ab57322f))


### Performance Improvements

* cut per-poll Docker and database work, compress frontend assets, and fix Compose merge keys ([#41](https://github.com/Crunging/Ludock/issues/41)) ([63bbdc0](https://github.com/Crunging/Ludock/commit/63bbdc0428df14d89b0747a2b24abcea9a9df03f))

## [0.4.0](https://github.com/Crunging/Ludock/compare/v0.3.0...v0.4.0) (2026-10-16)


### ⚠ BREAKING CHANGES

* Password authentication accepts Argon2id hashes only; legacy scrypt hashes no longer authenticate.

### Features

* automatically enable validated backup storage ([#33](https://github.com/Crunging/Ludock/issues/33)) ([3d2807f](https://github.com/Crunging/Ludock/commit/3d2807f4745216ad6e02c895beffce658f773d08))
* **frontend:** restyle the panel with pip states, sidebar icons, and IBM Plex ([#30](https://github.com/Crunging/Ludock/issues/30)) ([f11628b](https://github.com/Crunging/Ludock/commit/f11628b7f229d90b5fda386f282d0c8c03ee5ab6))
* redesign the panel around server status, addresses, and everyday tasks ([#32](https://github.com/Crunging/Ludock/issues/32)) ([f8cf1b0](https://github.com/Crunging/Ludock/commit/f8cf1b0e8ea1bb7a0d069e12c3453d68ea1bf3aa))
* **servers:** offer a backup before any restart, replacing Restart to update ([#39](https://github.com/Crunging/Ludock/issues/39)) ([264f230](https://github.com/Crunging/Ludock/commit/264f2302f9f1f988a4cfc5330813950b5adbc117))
* **updates:** add zero-setup restart-to-update support ([#34](https://github.com/Crunging/Ludock/issues/34)) ([9849097](https://github.com/Crunging/Ludock/commit/984909747f86d79fb879e48bb579591bc5f4eead))


### Bug Fixes

* address review findings and run lifecycle operations concurrently ([#40](https://github.com/Crunging/Ludock/issues/40)) ([c484d00](https://github.com/Crunging/Ludock/commit/c484d0085dd0b5be142e48591e700fc438d7a322))
* **ci:** stage Release Please assets under Bun ([#21](https://github.com/Crunging/Ludock/issues/21)) ([07246ef](https://github.com/Crunging/Ludock/commit/07246eff2c785cbc39022046caf975e351756845))
* **console:** run network commands in the game network namespace ([#35](https://github.com/Crunging/Ludock/issues/35)) ([8973b68](https://github.com/Crunging/Ludock/commit/8973b68f80d6eb812b85a8cf9c65282cd01c5797))
* handle log redaction and recovery edge cases ([#25](https://github.com/Crunging/Ludock/issues/25)) ([1f58d21](https://github.com/Crunging/Ludock/commit/1f58d216a4cae3097aa277744db7ec60717c1a7c))
* harden data operations against Docker failures before 0.4.0 ([#38](https://github.com/Crunging/Ludock/issues/38)) ([4364064](https://github.com/Crunging/Ludock/commit/43640644ed44b0e5dcd601c5ca9a79f50e8146af))
* harden runtimes without adding setup requirements ([#27](https://github.com/Crunging/Ludock/issues/27)) ([20aa79e](https://github.com/Crunging/Ludock/commit/20aa79e64c4ecbc595a04277c5dc3276798af4f0))
* harden server recovery and focus regression coverage ([#31](https://github.com/Crunging/Ludock/issues/31)) ([b4a40c7](https://github.com/Crunging/Ludock/commit/b4a40c778890c8e414afe19332f9a4cda2c83d52))
* make game server stops and restarts graceful ([#36](https://github.com/Crunging/Ludock/issues/36)) ([8bd3bf9](https://github.com/Crunging/Ludock/commit/8bd3bf92450c2110314750baa0f2edb7e250caef))
* **updates:** save the world before restart updates and use game stop grace periods for backups ([#37](https://github.com/Crunging/Ludock/issues/37)) ([0e52e4b](https://github.com/Crunging/Ludock/commit/0e52e4b7f1c38b44da1ffc821647824fae51cd90))


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
