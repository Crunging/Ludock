# Changelog

## [0.7.0](https://github.com/Crunging/Ludock/compare/v0.6.0...v0.7.0) (2026-11-21)


### Features

* a simpler Ludock: four menu entries, one Activity page, plain words, and nothing members can't use ([#117](https://github.com/Crunging/Ludock/issues/117)) ([bab6e02](https://github.com/Crunging/Ludock/commit/bab6e0251f0e6e806ad3ada98a83f8bbf556c542))
* add a server Ludock didn't find by itself from the server list, and remove one, without editing Compose ([#104](https://github.com/Crunging/Ludock/issues/104)) ([ea2c292](https://github.com/Crunging/Ludock/commit/ea2c292fe53fd08e01b6e37debc3f0ef95ef615c))
* add optional two-factor sign-in with an authenticator app ([#87](https://github.com/Crunging/Ludock/issues/87)) ([fd39d7b](https://github.com/Crunging/Ludock/commit/fd39d7b900f7cd9eeb5a68fa8ac1986faf1b1506))
* after setup, the server list shows the few things left to do, and does them in a click ([#112](https://github.com/Crunging/Ludock/issues/112)) ([9ecca22](https://github.com/Crunging/Ludock/commit/9ecca2219a7cc9ba1c5f2d2a5b4a045bc1c67a15))
* alert when a game stops answering, and count players on games without a console password ([#92](https://github.com/Crunging/Ludock/issues/92)) ([260c043](https://github.com/Crunging/Ludock/commit/260c0430ba8f1e0617d482406bf723ebf01d671a))
* alert when a server's disk is nearly full or its memory stays near its limit ([#96](https://github.com/Crunging/Ludock/issues/96)) ([3107d57](https://github.com/Crunging/Ludock/commit/3107d57d7253a24946580fb18aaaf5d3d616a839))
* browse a backup and restore just one file or folder from it ([#101](https://github.com/Crunging/Ludock/issues/101)) ([003e79a](https://github.com/Crunging/Ludock/commit/003e79af257678bf1babe3acea647229510219cd))
* calmer server pages: who's on comes first, Docker details and filters fold away, and dialogs say who a stop disconnects ([#153](https://github.com/Crunging/Ludock/issues/153)) ([cad7a63](https://github.com/Crunging/Ludock/commit/cad7a63ce2f5788b277dd2adf9e39fb3c550c2a4))
* cancel a restart, stop, update, or scheduled run while Ludock is still waiting, and players hear it's off ([#128](https://github.com/Crunging/Ludock/issues/128)) ([b23fcce](https://github.com/Crunging/Ludock/commit/b23fccecf99b2f3d22421e26ec65fa5bda0f4d84))
* choose which types of alert each notification destination gets, with newer-image notices off by default ([#103](https://github.com/Crunging/Ludock/issues/103)) ([b88ab83](https://github.com/Crunging/Ludock/commit/b88ab83b22ea0396e89183b899f6756f749ba9b5))
* edit text files in the browser from the file preview ([#88](https://github.com/Crunging/Ludock/issues/88)) ([1903a0e](https://github.com/Crunging/Ludock/commit/1903a0eb64386bb7a6575657d5e9c803bc16eed3))
* invite users and reset passwords with one-time links instead of sharing passwords ([#99](https://github.com/Crunging/Ludock/issues/99)) ([15c9584](https://github.com/Crunging/Ludock/commit/15c9584a813e5a6655766ddff3c2797808162420))
* keep a second copy of every backup on another disk, and restore from it if the first is lost ([#109](https://github.com/Crunging/Ludock/issues/109)) ([33368ad](https://github.com/Crunging/Ludock/commit/33368ad530177371b4995139801ce3da75522bb5))
* keep weeks to months of backups with nothing to configure, and let administrators keep a backup for good ([#106](https://github.com/Crunging/Ludock/issues/106)) ([2c74eed](https://github.com/Crunging/Ludock/commit/2c74eed2efbd8b59d272362f9a1b67d60f88bf97))
* kick, ban, and allow players from the server page, without typing console commands ([#110](https://github.com/Crunging/Ludock/issues/110)) ([fb06d68](https://github.com/Crunging/Ludock/commit/fb06d68eed445e099e39e122e8d617d39096c7ea))
* let scheduled restarts and stops wait until no one is online ([#93](https://github.com/Crunging/Ludock/issues/93)) ([d35a799](https://github.com/Crunging/Ludock/commit/d35a7996457f7e7daa6319d2a542652b0e152fb9))
* lighter Settings and Backups pages, second copies to a mounted folder without asking, and one-file restores that back up first ([#121](https://github.com/Crunging/Ludock/issues/121)) ([5ad357a](https://github.com/Crunging/Ludock/commit/5ad357a00b07e3bb8e6d6cf5a3341f668d5ceeaa))
* Ludock saves its own data beside your backups every day, and restores it with one command ([#116](https://github.com/Crunging/Ludock/issues/116)) ([3971a41](https://github.com/Crunging/Ludock/commit/3971a4144907cbdd8dc306813738e4312a46f7ac))
* players are warned and waited for before Ludock restarts, stops, backs up, or updates their server ([#120](https://github.com/Crunging/Ludock/issues/120)) ([cd1c66c](https://github.com/Crunging/Ludock/commit/cd1c66ca44cb20b57b25693c0c7ff2921655e747))
* recognize Satisfactory, Enshrouded, Minecraft Bedrock, Don't Starve Together, Core Keeper, and Vintage Story servers ([#95](https://github.com/Crunging/Ludock/issues/95)) ([e61524b](https://github.com/Crunging/Ludock/commit/e61524b76a64f016b9cfce31e20c71a2d5573705))
* redesign the panel with light and dark themes on htmx 4 ([#86](https://github.com/Crunging/Ludock/issues/86)) ([9af5f42](https://github.com/Crunging/Ludock/commit/9af5f42758d8ea649602da091a60f5edbf343c24))
* restart a server whose game stopped answering, so a hung server comes back without waiting for someone to wake up ([#115](https://github.com/Crunging/Ludock/issues/115)) ([6911d59](https://github.com/Crunging/Ludock/commit/6911d593ff01087e80f55378007d62d064688920))
* restore older backups after moving a server's data or rebuilding the host ([#122](https://github.com/Crunging/Ludock/issues/122)) ([4059527](https://github.com/Crunging/Ludock/commit/40595272706ff12592939cdd92a2be061fd0850f))
* run Ludock from one Compose file with nothing to configure, and set the upload limit, verbose logging, and reverse proxy in Settings ([#111](https://github.com/Crunging/Ludock/issues/111)) ([8db379b](https://github.com/Crunging/Ludock/commit/8db379b2f9de8973928c31b52455a3c7afcde9ad))
* schedule console commands, such as a recurring save-all or announcement ([#90](https://github.com/Crunging/Ludock/issues/90)) ([6aa0645](https://github.com/Crunging/Ludock/commit/6aa0645edd9891740f9ef99b85825f400ff63991))
* share a server with a friend from its page in one step, choosing whether they can see it, run it, or have full access ([#108](https://github.com/Crunging/Ludock/issues/108)) ([5aaafe4](https://github.com/Crunging/Ludock/commit/5aaafe46020e1a8309de8286b57e420152f1a6b0))
* show who's online on Factorio, Palworld, ARK, Project Zomboid, Conan Exiles, and 7 Days to Die servers ([#89](https://github.com/Crunging/Ludock/issues/89)) ([46ec721](https://github.com/Crunging/Ludock/commit/46ec721bdfa4055dc06b5929618c822530484c97))
* tell administrators in Settings when a newer Ludock image is available, with the commands that update it ([#113](https://github.com/Crunging/Ludock/issues/113)) ([fcc0233](https://github.com/Crunging/Ludock/commit/fcc0233e850e1657059d5ac512dfa0faac1099e1))
* tell administrators when a server's image has a newer version ([#91](https://github.com/Crunging/Ludock/issues/91)) ([120f9ed](https://github.com/Crunging/Ludock/commit/120f9ed2e4a62a9616d31d56a049fe6076a57b18))
* Terraria, Bedrock, and Vintage Story list who's online, Palworld backs up only its saves and warns players, and health checks catch hung games ([#148](https://github.com/Crunging/Ludock/issues/148)) ([4e9015a](https://github.com/Crunging/Ludock/commit/4e9015af9341e32401565d59cb9af33df302e6eb))
* unpack uploaded archives and select several files to delete, move, or download at once ([#105](https://github.com/Crunging/Ludock/issues/105)) ([7dc9519](https://github.com/Crunging/Ludock/commit/7dc951999425cc8333adc51bbbb51de5dcd4ee85))
* watch every server from the start, and open forms on the common choice ([#107](https://github.com/Crunging/Ludock/issues/107)) ([0a543ae](https://github.com/Crunging/Ludock/commit/0a543aef6ee6cbcb5a002fa1291422e3324d58fc))


### Bug Fixes

* a backup or restore cut by a dropped Docker connection waits or says so, instead of blaming the files or the backup ([#154](https://github.com/Crunging/Ludock/issues/154)) ([e548bc5](https://github.com/Crunging/Ludock/commit/e548bc5da1773031111b769cf93d3455d6996e9c))
* a game that stopped answering is still caught after Ludock restarts ([#152](https://github.com/Crunging/Ludock/issues/152)) ([7301c22](https://github.com/Crunging/Ludock/commit/7301c22870938b60ef9abbd360b59aca976d7eee))
* a nightly backup or restart survives Docker restarting for a few seconds, an interrupted restart with a backup brings the server back, and old work no longer piles up ([#142](https://github.com/Crunging/Ludock/issues/142)) ([883ad70](https://github.com/Crunging/Ludock/commit/883ad708042d4d390e456fbb777a53531d41fca0))
* a restore stopped partway can always be finished or undone, warnings never claim to have gone out when they didn't, and upgrading from 0.6.0 keeps sharing and backups working ([#156](https://github.com/Crunging/Ludock/issues/156)) ([c0b60f3](https://github.com/Crunging/Ludock/commit/c0b60f3013b1278fc6cfc5588fac21c2aa02234e))
* a restore stops if the server is started while its data is being swapped ([#151](https://github.com/Crunging/Ludock/issues/151)) ([e9ce05d](https://github.com/Crunging/Ludock/commit/e9ce05d930e946cc68c6a01df0518cb3362bbc37))
* a server that went down says so everywhere, Activity lists each piece of work once in plain words, and alerts skip what you just did ([#119](https://github.com/Crunging/Ludock/issues/119)) ([32272ef](https://github.com/Crunging/Ludock/commit/32272ef1dd105fcd2477f0a5d7892dc1dea486d1))
* backups and restores warn players first, Needs attention forgets failures a retry fixed, and file actions no longer hang on Docker ([#136](https://github.com/Crunging/Ludock/issues/136)) ([19f2fbe](https://github.com/Crunging/Ludock/commit/19f2fbe90dcd16430e27b740ba68536c211283b2))
* **backups:** release the second folder before reporting its work done ([#114](https://github.com/Crunging/Ludock/issues/114)) ([1e0dc46](https://github.com/Crunging/Ludock/commit/1e0dc46b55ede0a2613102e308095d2c04287ab4))
* correct eight edge cases in files, alerts, schedules, players, and sign-in ([#100](https://github.com/Crunging/Ludock/issues/100)) ([e4e86e8](https://github.com/Crunging/Ludock/commit/e4e86e849c42400d39a6eb8d2604c869d5b5237b))
* failures say what went wrong in plain words, blame Docker when it was Docker, and a stopped server highlights Start ([#146](https://github.com/Crunging/Ludock/issues/146)) ([66efb6d](https://github.com/Crunging/Ludock/commit/66efb6da86dd25a509e0d681ae7543587ab56511))
* file changes aren't reported as failed when Docker confirms late, a quick click after a delete can't hit the wrong file, and a restore warns players who joined while it checked the backup ([#147](https://github.com/Crunging/Ludock/issues/147)) ([41f222c](https://github.com/Crunging/Ludock/commit/41f222ca16203d4496f69477e91e1048cfce0d60))
* games that can control the host join Ludock only when their owner labels them in ([#125](https://github.com/Crunging/Ludock/issues/125)) ([a4a3711](https://github.com/Crunging/Ludock/commit/a4a371107c2d712f93d43f971ff936f910eeac8e))
* keep servers usable when you change their containers in Docker; only a new game or data folder asks to confirm sharing ([#102](https://github.com/Crunging/Ludock/issues/102)) ([d2f6992](https://github.com/Crunging/Ludock/commit/d2f6992aab0f1e282a7aec0f1ceb045cdf22eb4e))
* Ludock calls each thing by one name, Activity can cancel waiting work, and the docs match the panel ([#150](https://github.com/Crunging/Ludock/issues/150)) ([788d485](https://github.com/Crunging/Ludock/commit/788d4856cac6793445bc5f8c77f6f6f72b3ead7c))
* Ludock stops within seconds, a stuck backup folder no longer holds up the panel, and backups on a lost disk can be removed ([#126](https://github.com/Crunging/Ludock/issues/126)) ([073630b](https://github.com/Crunging/Ludock/commit/073630b1333974a325750396a45f28c89979585a))
* Ludock survives Docker going away mid-restart, says plainly when Docker is the problem, and tabs never reload the page ([#135](https://github.com/Crunging/Ludock/issues/135)) ([f478ea8](https://github.com/Crunging/Ludock/commit/f478ea80fe66776b12779accc0a6035ecdb6829e))
* Ludock tells you when alerts aren't arriving, a test shows its result where you clicked, and each delivery shows the alert it sent ([#144](https://github.com/Crunging/Ludock/issues/144)) ([18f380e](https://github.com/Crunging/Ludock/commit/18f380e3757b69d6facfd41203227602a3d2b4b6))
* one game whose status check hangs no longer slows every other server's player count and health ([#157](https://github.com/Crunging/Ludock/issues/157)) ([45e2d0e](https://github.com/Crunging/Ludock/commit/45e2d0e8c4b7216fe20d2b25545e616ccacfd7a9))
* players warned of a scheduled restart hear it's off when the schedule is paused, changed, or deleted, or the run is dropped ([#131](https://github.com/Crunging/Ludock/issues/131)) ([d839273](https://github.com/Crunging/Ludock/commit/d839273ad6df134cfe034f2907edea94e475d5d8))
* show a finished operation on the server list right away ([#94](https://github.com/Crunging/Ludock/issues/94)) ([48d09a6](https://github.com/Crunging/Ludock/commit/48d09a688417d944013482bc27096aa37d2648d3))
* stop backups, schedules, and restores failing quietly, and cut idle Docker work ([#83](https://github.com/Crunging/Ludock/issues/83)) ([63904db](https://github.com/Crunging/Ludock/commit/63904dba775d3f9058fe9419d4601a9636efd16b))
* Terraria, Factorio, Valheim, and Palworld servers back up, talk, and list players on their real images ([#138](https://github.com/Crunging/Ludock/issues/138)) ([6bcff88](https://github.com/Crunging/Ludock/commit/6bcff88da6e99aaad27a7cc71a15685263830f2f))
* updates and docker compose pull no longer lose the server, and backups save the world first ([#133](https://github.com/Crunging/Ludock/issues/133)) ([ea40b12](https://github.com/Crunging/Ludock/commit/ea40b127a7026c8035b1d4f34bb0375ae9b28a23))
* warnings before a restart, backup or update go to whoever is online when it runs, and backup settings no longer hang on a stuck folder or Docker ([#141](https://github.com/Crunging/Ludock/issues/141)) ([68459f0](https://github.com/Crunging/Ludock/commit/68459f0689cd39851bc2d2fb8923011db21d828b))


### Performance Improvements

* finishing an operation finds its Activity entry by operation, not by reading the server's whole history ([#130](https://github.com/Crunging/Ludock/issues/130)) ([7bb3fd2](https://github.com/Crunging/Ludock/commit/7bb3fd28d9eb92789aca3a6acecbdbdd48ceda58))
* Ludock stops writing to your game's log, file previews no longer start a container each, and a server page reads only its own server ([#139](https://github.com/Crunging/Ludock/issues/139)) ([a7c3d8d](https://github.com/Crunging/Ludock/commit/a7c3d8d9595d0ffe2a74f88a9331e0f31bc7f75b))


### Code Refactoring

* background server reads and admin-only renders each confirm access through one helper ([#134](https://github.com/Crunging/Ludock/issues/134)) ([bd3dca0](https://github.com/Crunging/Ludock/commit/bd3dca0a254c5dfe15550c55aee7a01d581658bc))
* background work, settings reads, and the yielding lock acquire each have one shared helper ([#132](https://github.com/Crunging/Ludock/issues/132)) ([86e0a35](https://github.com/Crunging/Ludock/commit/86e0a3573546127c4aea8b47f9d6f4c81adfa045))
* file and backup helper commands share one implementation, so the next fix to an uncertain command lands once ([#143](https://github.com/Crunging/Ludock/issues/143)) ([07ea5dd](https://github.com/Crunging/Ludock/commit/07ea5ddabff426f7a6cbf87759d8e380ab7336bd))
* remove unreachable code and unused styles ([#129](https://github.com/Crunging/Ludock/issues/129)) ([2699841](https://github.com/Crunging/Ludock/commit/269984157d6ff978a6b406b7512cee79635b076d))
* the restore reads as its phases in order, with tests pinning each journal save and check ([#149](https://github.com/Crunging/Ludock/issues/149)) ([87eb89e](https://github.com/Crunging/Ludock/commit/87eb89ec4cbe2549cb5baf4ef834010b1475c392))
* the restore-extract format and the stalled-folder guard each have one definition ([#137](https://github.com/Crunging/Ludock/issues/137)) ([dffb687](https://github.com/Crunging/Ludock/commit/dffb6873251dac82bf5124b302c735b5e5e26170))
* the server table says "sharing paused" and stops keeping a binding history nobody reads ([#140](https://github.com/Crunging/Ludock/issues/140)) ([3802d5d](https://github.com/Crunging/Ludock/commit/3802d5d2802473a541ccb52ce409c0cc2da5484a))

## [0.6.0](https://github.com/Crunging/Ludock/compare/v0.5.0...v0.6.0) (2026-11-01)


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
* correct six bugs found by testing the main flows ([#80](https://github.com/Crunging/Ludock/issues/80)) ([cc0c579](https://github.com/Crunging/Ludock/commit/cc0c579addbe7fcbc967a157a32278f35d7f6123))
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
