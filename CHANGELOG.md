# Changelog

## [0.7.0](https://github.com/Crunging/Ludock/compare/v0.6.0...v0.7.0) (2026-11-27)


### Features

* a renamed server can take over its old backups, schedules and sharing ([#242](https://github.com/Crunging/Ludock/issues/242)) ([fcef672](https://github.com/Crunging/Ludock/commit/fcef672f9190566e1a55651796e4d481c3fe45fb))
* a simpler Ludock: four menu entries, one Activity page, plain words, and nothing members can't use ([#117](https://github.com/Crunging/Ludock/issues/117)) ([bab6e02](https://github.com/Crunging/Ludock/commit/bab6e0251f0e6e806ad3ada98a83f8bbf556c542))
* add a server Ludock didn't find by itself from the server list, and remove one, without editing Compose ([#104](https://github.com/Crunging/Ludock/issues/104)) ([ea2c292](https://github.com/Crunging/Ludock/commit/ea2c292fe53fd08e01b6e37debc3f0ef95ef615c))
* add optional two-factor sign-in with an authenticator app ([#87](https://github.com/Crunging/Ludock/issues/87)) ([fd39d7b](https://github.com/Crunging/Ludock/commit/fd39d7b900f7cd9eeb5a68fa8ac1986faf1b1506))
* after setup, the server list shows the few things left to do, and does them in a click ([#112](https://github.com/Crunging/Ludock/issues/112)) ([9ecca22](https://github.com/Crunging/Ludock/commit/9ecca2219a7cc9ba1c5f2d2a5b4a045bc1c67a15))
* alert when a game stops answering, and count players on games without a console password ([#92](https://github.com/Crunging/Ludock/issues/92)) ([260c043](https://github.com/Crunging/Ludock/commit/260c0430ba8f1e0617d482406bf723ebf01d671a))
* alert when a server's disk is nearly full or its memory stays near its limit ([#96](https://github.com/Crunging/Ludock/issues/96)) ([3107d57](https://github.com/Crunging/Ludock/commit/3107d57d7253a24946580fb18aaaf5d3d616a839))
* an unowned Ludock can be set up without a code for its first 10 minutes ([#309](https://github.com/Crunging/Ludock/issues/309)) ([e5efc85](https://github.com/Crunging/Ludock/commit/e5efc85af172affad59174360072c3516857c3b8))
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
* Settings → System shows which version of Ludock is running ([#208](https://github.com/Crunging/Ludock/issues/208)) ([e6030bd](https://github.com/Crunging/Ludock/commit/e6030bd40a21bb6015371948bed95c79abae9f4d))
* share a server with a friend from its page in one step, choosing whether they can see it, run it, or have full access ([#108](https://github.com/Crunging/Ludock/issues/108)) ([5aaafe4](https://github.com/Crunging/Ludock/commit/5aaafe46020e1a8309de8286b57e420152f1a6b0))
* show who's online on Factorio, Palworld, ARK, Project Zomboid, Conan Exiles, and 7 Days to Die servers ([#89](https://github.com/Crunging/Ludock/issues/89)) ([46ec721](https://github.com/Crunging/Ludock/commit/46ec721bdfa4055dc06b5929618c822530484c97))
* tell administrators in Settings when a newer Ludock image is available, with the commands that update it ([#113](https://github.com/Crunging/Ludock/issues/113)) ([fcc0233](https://github.com/Crunging/Ludock/commit/fcc0233e850e1657059d5ac512dfa0faac1099e1))
* tell administrators when a server's image has a newer version ([#91](https://github.com/Crunging/Ludock/issues/91)) ([120f9ed](https://github.com/Crunging/Ludock/commit/120f9ed2e4a62a9616d31d56a049fe6076a57b18))
* Terraria, Bedrock, and Vintage Story list who's online, Palworld backs up only its saves and warns players, and health checks catch hung games ([#148](https://github.com/Crunging/Ludock/issues/148)) ([4e9015a](https://github.com/Crunging/Ludock/commit/4e9015af9341e32401565d59cb9af33df302e6eb))
* unpack uploaded archives and select several files to delete, move, or download at once ([#105](https://github.com/Crunging/Ludock/issues/105)) ([7dc9519](https://github.com/Crunging/Ludock/commit/7dc951999425cc8333adc51bbbb51de5dcd4ee85))
* update Ludock from Settings in one click, with the old version put back if the new one doesn't come up ([#284](https://github.com/Crunging/Ludock/issues/284)) ([676efad](https://github.com/Crunging/Ludock/commit/676efad1dc02423ff11948e42df82f76beeb5337))
* watch every server from the start, and open forms on the common choice ([#107](https://github.com/Crunging/Ludock/issues/107)) ([0a543ae](https://github.com/Crunging/Ludock/commit/0a543aef6ee6cbcb5a002fa1291422e3324d58fc))
* webhook alerts carry their alert type, so scripts don't have to read the sentence ([#203](https://github.com/Crunging/Ludock/issues/203)) ([ebfaf4f](https://github.com/Crunging/Ludock/commit/ebfaf4faaef7b48453312e6855e08310cabca3e9))


### Bug Fixes

* "Outage since" says when the server stopped, not when Ludock noticed ([#272](https://github.com/Crunging/Ludock/issues/272)) ([65b8315](https://github.com/Crunging/Ludock/commit/65b8315b73b8cfb7fb5cf5fd981a4c2e617794cf))
* / holds every path, so mounting the host root into Ludock keeps game folders out of Files ([#197](https://github.com/Crunging/Ludock/issues/197)) ([fd4ad28](https://github.com/Crunging/Ludock/commit/fd4ad28d98e18f0f92e720e31b255e9961aa5b74))
* 1.0 starts fresh: a beta's data is refused untouched, and a beta's backups and saves are never removed ([#166](https://github.com/Crunging/Ludock/issues/166)) ([e4222b8](https://github.com/Crunging/Ludock/commit/e4222b8466f581cb98c5f911f1c95f55193e6bb6))
* a backup or restore cut by a dropped Docker connection waits or says so, instead of blaming the files or the backup ([#154](https://github.com/Crunging/Ludock/issues/154)) ([e548bc5](https://github.com/Crunging/Ludock/commit/e548bc5da1773031111b769cf93d3455d6996e9c))
* a backup or restore never crosses into another disk mounted inside a server's data, and one that can't work leaves the server running ([#180](https://github.com/Crunging/Ludock/issues/180)) ([020df4c](https://github.com/Crunging/Ludock/commit/020df4cfffff76f1e3b1a2758f2079ab6f5d9577))
* a backup that can't fit never stops the server, and a download Docker cut says Docker failed ([#187](https://github.com/Crunging/Ludock/issues/187)) ([7675c56](https://github.com/Crunging/Ludock/commit/7675c5690f0b7e2a178156e1dd86b71c500e30bb))
* a backup that needs room makes it first instead of stopping the server and failing ([#222](https://github.com/Crunging/Ludock/issues/222)) ([ff369aa](https://github.com/Crunging/Ludock/commit/ff369aad566b90e9cf1a393dbb4b492f0c64f0f6))
* a backup whose only intact copy is out of reach is no longer offered for a restore ([#201](https://github.com/Crunging/Ludock/issues/201)) ([62b006c](https://github.com/Crunging/Ludock/commit/62b006ca371b7c85937e8c6c599245132df96d5c))
* a crash loop alerts on time, says when it's back after a Ludock stop, and doesn't flicker between restarts ([#264](https://github.com/Crunging/Ludock/issues/264)) ([2705646](https://github.com/Crunging/Ludock/commit/2705646cae458333da3b32b9a3571290442be499))
* a crash-looping server no longer flickers to "Starting" with Restart as a run ends ([#281](https://github.com/Crunging/Ludock/issues/281)) ([be66623](https://github.com/Crunging/Ludock/commit/be6662316723f7ca3509fadb7079c2c3ade81bb3))
* a damaged backup archive no longer takes the place of one that can be restored ([#198](https://github.com/Crunging/Ludock/issues/198)) ([4b9a44a](https://github.com/Crunging/Ludock/commit/4b9a44a615f87469c5434224c8ea997daedc49a6))
* a demoted administrator's settings change shows no settings ([#177](https://github.com/Crunging/Ludock/issues/177)) ([ce202f2](https://github.com/Crunging/Ludock/commit/ce202f2fd68c642367c976c5643fed2dbd1e368e))
* a Docker Engine that is too old says so instead of looking unreachable ([#205](https://github.com/Crunging/Ludock/issues/205)) ([7b53154](https://github.com/Crunging/Ludock/commit/7b5315401bf668269b32a02caf90df8ea97bc01d))
* a failed Ludock update can't undo a revocation, and its data copy is never readable by others ([#292](https://github.com/Crunging/Ludock/issues/292)) ([a10c04f](https://github.com/Crunging/Ludock/commit/a10c04f3fe9346e3c72b5cefd71deae89d879b07))
* a failed Ludock update reports its data truthfully after a restart, and starts don't wait on Docker without an update ([#300](https://github.com/Crunging/Ludock/issues/300)) ([c895f39](https://github.com/Crunging/Ludock/commit/c895f3905c86b7ae0bd12e3b83630e56d3373a33))
* a game that stopped answering is still caught after Ludock restarts ([#152](https://github.com/Crunging/Ludock/issues/152)) ([7301c22](https://github.com/Crunging/Ludock/commit/7301c22870938b60ef9abbd360b59aca976d7eee))
* a helper image older than Ludock is refused before it can back up, restore, or upload ([#293](https://github.com/Crunging/Ludock/issues/293)) ([23e47aa](https://github.com/Crunging/Ludock/commit/23e47aa573e216bda7ebddf8efef6aa1d00c16e5))
* a member sees a paused server as paused, and a removed member that their access ended ([#285](https://github.com/Crunging/Ludock/issues/285)) ([ebbfffa](https://github.com/Crunging/Ludock/commit/ebbfffa912bbf2089efdc8cb18d96df3009f9cd6))
* a member's changes wait for a Ludock update too, and test-only account helpers move to dbtest ([#296](https://github.com/Crunging/Ludock/issues/296)) ([6e8bdec](https://github.com/Crunging/Ludock/commit/6e8bdecb62f023e4c578d45ebca2f44d729d4544))
* a missing server's backups can't be stranded by removing it from Ludock ([#249](https://github.com/Crunging/Ludock/issues/249)) ([d6d2604](https://github.com/Crunging/Ludock/commit/d6d260407431938cfbf50692f788b625cc10f965))
* a nested mod pack uploads in as few requests as a flat one, not one per folder ([#265](https://github.com/Crunging/Ludock/issues/265)) ([7cd4606](https://github.com/Crunging/Ludock/commit/7cd460675617e5d1f72cf42a33f07db231850892))
* a newly opened page gets its players as soon as other work frees the server, and hung games no longer stall the healthy ones ([#159](https://github.com/Crunging/Ludock/issues/159)) ([7cbfb1d](https://github.com/Crunging/Ludock/commit/7cbfb1d046e4ab9a3026b7e75134a80ba424babb))
* a nightly backup or restart survives Docker restarting for a few seconds, an interrupted restart with a backup brings the server back, and old work no longer piles up ([#142](https://github.com/Crunging/Ludock/issues/142)) ([883ad70](https://github.com/Crunging/Ludock/commit/883ad708042d4d390e456fbb777a53531d41fca0))
* a reboot during a Ludock update brings the old version back by itself, and Settings says only what is still true ([#301](https://github.com/Crunging/Ludock/issues/301)) ([a2f1ad9](https://github.com/Crunging/Ludock/commit/a2f1ad9205f11bd14e476de345f54983231493a2))
* a removed server's page says it was removed, not that Docker lost it, and a few messages fit who reads them ([#271](https://github.com/Crunging/Ludock/issues/271)) ([1d4de27](https://github.com/Crunging/Ludock/commit/1d4de27a6c879cc0b17ab57fec8a7c9948fc57f1))
* a restart can't lose an alert, and a signed-out session can't change a schedule ([#179](https://github.com/Crunging/Ludock/issues/179)) ([69a1447](https://github.com/Crunging/Ludock/commit/69a1447f20d6d06697df1fb88ca7d86d6e3a55df))
* a restart cut short by Ludock's own restart no longer leaves the server down ([#262](https://github.com/Crunging/Ludock/issues/262)) ([3486aee](https://github.com/Crunging/Ludock/commit/3486aee412a6c49cc026438a990a7fff99e13449))
* a restart that backs up first no longer takes the game offline when an earlier restore can't be finished ([#247](https://github.com/Crunging/Ludock/issues/247)) ([43ed0b7](https://github.com/Crunging/Ludock/commit/43ed0b70b4f338ba23c9ad4a6e75daf7898ff7d0))
* a restart with a backup leaves a server stopped outside Ludock meanwhile stopped ([#183](https://github.com/Crunging/Ludock/issues/183)) ([9e552b1](https://github.com/Crunging/Ludock/commit/9e552b19c120c7be13bd5976021531ef4794379b))
* a restore and a folder delete on unusual storage say what really happened ([#303](https://github.com/Crunging/Ludock/issues/303)) ([a967183](https://github.com/Crunging/Ludock/commit/a967183b5720dd97d913fa1a65e03b56e4b30304))
* a restore no longer destroys another restore's saved previous data ([#195](https://github.com/Crunging/Ludock/issues/195)) ([2282615](https://github.com/Crunging/Ludock/commit/2282615fbc802d2621c786f08a162a564d2a3a0f))
* a restore stopped partway can always be finished or undone, warnings never claim to have gone out when they didn't, and upgrading from 0.6.0 keeps sharing and backups working ([#156](https://github.com/Crunging/Ludock/issues/156)) ([c0b60f3](https://github.com/Crunging/Ludock/commit/c0b60f3013b1278fc6cfc5588fac21c2aa02234e))
* a restore stops if the server is started while its data is being swapped ([#151](https://github.com/Crunging/Ludock/issues/151)) ([e9ce05d](https://github.com/Crunging/Ludock/commit/e9ce05d930e946cc68c6a01df0518cb3362bbc37))
* a restore stops moving data within about a second of the game being started outside Ludock ([#259](https://github.com/Crunging/Ludock/issues/259)) ([a49e3a7](https://github.com/Crunging/Ludock/commit/a49e3a7b7df72bb43858b9bbbe38b5605eab1691))
* a restore that fails for a broken backup or lost access says so, not that the disk is full ([#232](https://github.com/Crunging/Ludock/issues/232)) ([df5be79](https://github.com/Crunging/Ludock/commit/df5be79abeeaa47749d4d5d6b072dfc8c8b537f9))
* a restore that was undone stops asking for attention once the server's next work succeeds ([#221](https://github.com/Crunging/Ludock/issues/221)) ([8f65269](https://github.com/Crunging/Ludock/commit/8f652691e7bbba0dca1be38c0cfa606decc74484))
* a restore undone because something wrote into the data says so, and Activity names the set-aside folder in the right tense ([#304](https://github.com/Crunging/Ludock/issues/304)) ([911d942](https://github.com/Crunging/Ludock/commit/911d94270562ba5b3d4200cd911a8876dd063475))
* a restored file that had no ACL no longer picks up the data folder's default ACL ([#269](https://github.com/Crunging/Ludock/issues/269)) ([d867076](https://github.com/Crunging/Ludock/commit/d8670767ed8214f97a17ae46d13553a07abb55f0))
* a restored folder keeps its setgid and sticky bits ([#224](https://github.com/Crunging/Ludock/issues/224)) ([d938749](https://github.com/Crunging/Ludock/commit/d9387496fa9e6a9d93064fc4e653ccc280dcda87))
* a schedule that can't run says it is suspended, and stops counting as a backup ([#215](https://github.com/Crunging/Ludock/issues/215)) ([ca05e3c](https://github.com/Crunging/Ludock/commit/ca05e3cb51609a13936f42a543324e27717f1be3))
* a server missing in Docker is listed once in Needs attention, not as down too ([#289](https://github.com/Crunging/Ludock/issues/289)) ([a00b4e3](https://github.com/Crunging/Ludock/commit/a00b4e37180fc75d31287c9634f5d2c88d577acf))
* a server started from Ludock after an outage says it is back up ([#213](https://github.com/Crunging/Ludock/issues/213)) ([a208fad](https://github.com/Crunging/Ludock/commit/a208fad87d317548fde9e46f5ac70a447e2deb28))
* a server started from the panel shows in Activity, like a stop or restart ([#238](https://github.com/Crunging/Ludock/issues/238)) ([90fe6e6](https://github.com/Crunging/Ludock/commit/90fe6e6bcc06e1dd8442f4035eb04f2840a0f305))
* a server stopped outside Ludock says what to do on its page, and its recovery alert says it is running again ([#305](https://github.com/Crunging/Ludock/issues/305)) ([4be0e8e](https://github.com/Crunging/Ludock/commit/4be0e8ed61df2d1c624112b648e50dcdf9a7c8b1))
* a server stuck restarting can be stopped, backed up, restored, and undone from Ludock ([#256](https://github.com/Crunging/Ludock/issues/256)) ([566c1e6](https://github.com/Crunging/Ludock/commit/566c1e68d060a165a434c74b434e471b24be1886))
* a server that is back up says so at once, even right after Ludock started it ([#216](https://github.com/Crunging/Ludock/issues/216)) ([f969117](https://github.com/Crunging/Ludock/commit/f969117ebf006e71c5724fd24601b3c8bc050c80))
* a server that is gone keeps its backups within reach ([#207](https://github.com/Crunging/Ludock/issues/207)) ([d3966a4](https://github.com/Crunging/Ludock/commit/d3966a49d8ac43a089e2e8003b0538b064011afc))
* a server that went down says so everywhere, Activity lists each piece of work once in plain words, and alerts skip what you just did ([#119](https://github.com/Crunging/Ludock/issues/119)) ([32272ef](https://github.com/Crunging/Ludock/commit/32272ef1dd105fcd2477f0a5d7892dc1dea486d1))
* a server with no data folders is never offered a backup it can't take ([#196](https://github.com/Crunging/Ludock/issues/196)) ([8fc09ac](https://github.com/Crunging/Ludock/commit/8fc09ac94795dcb8470bb28061f65cc5bf03bcdc))
* a server with no data folders refuses a backup instead of queueing one that fails ([#200](https://github.com/Crunging/Ludock/issues/200)) ([ad31689](https://github.com/Crunging/Ludock/commit/ad3168984ea7882eff22b70c29372a0224ed7bcf))
* a server's Activity leads an administrator to the outages Ludock noticed ([#313](https://github.com/Crunging/Ludock/issues/313)) ([3ad40f9](https://github.com/Crunging/Ludock/commit/3ad40f9cbf221fab507f69e426ede3acf20ef3b8))
* a single-file undo rechecks its original after moving a clash aside, as a whole-folder undo does ([#258](https://github.com/Crunging/Ludock/issues/258)) ([0742010](https://github.com/Crunging/Ludock/commit/07420106579a8d06fa348a694c0cb63062113eff))
* a suspended schedule no longer uses up a countdown warning it can't send ([#229](https://github.com/Crunging/Ludock/issues/229)) ([2b2e50a](https://github.com/Crunging/Ludock/commit/2b2e50af6d4c6f972cd155fced8703169ad6d2f5))
* a suspended schedule's edit note and checklist item say one thing each ([#218](https://github.com/Crunging/Ludock/issues/218)) ([c88a0ac](https://github.com/Crunging/Ludock/commit/c88a0aca075ca00a55d4e0555754bb625bff9ab4))
* a whole restore brings back each data folder's own owner and permissions, and a save never replaces another installation's ([#174](https://github.com/Crunging/Ludock/issues/174)) ([87d715c](https://github.com/Crunging/Ludock/commit/87d715cf62b04ddb1547c3101b618d8ca4bc19b5))
* a whole restore names the folders Ludock set aside earlier before replacing them ([#286](https://github.com/Crunging/Ludock/issues/286)) ([1e9e8a1](https://github.com/Crunging/Ludock/commit/1e9e8a1018848333a7a77660a9d1561d865744c6))
* Activity says what an undo found and what to do when it can't go on ([#275](https://github.com/Crunging/Ludock/issues/275)) ([4b36a5c](https://github.com/Crunging/Ludock/commit/4b36a5ca51792bce69a7b3624f2e315457daec99))
* addresses in console and log output are no longer links, so a look-alike can't be clicked ([#233](https://github.com/Crunging/Ludock/issues/233)) ([2ac4ad1](https://github.com/Crunging/Ludock/commit/2ac4ad1dd3837a17cdcb6e91b79b628cbe9e5113))
* an alert no longer fails on a connection the receiver already closed ([#250](https://github.com/Crunging/Ludock/issues/250)) ([10f573d](https://github.com/Crunging/Ludock/commit/10f573d501897bb44c3e3f7f9c0a5108b9c41ec5))
* an edit link to someone else's schedule says whose it is instead of calling it gone ([#235](https://github.com/Crunging/Ludock/issues/235)) ([0ca88aa](https://github.com/Crunging/Ludock/commit/0ca88aac4b05c8fb5b1f8778dd70ae35b5aee53e))
* an interrupted backup is dated by when it started, as Activity dates it ([#283](https://github.com/Crunging/Ludock/issues/283)) ([df24cef](https://github.com/Crunging/Ludock/commit/df24cef6c6b0b4fcdd383ec6dfb008886ed306bd))
* an interrupted backup's Activity says what Ludock did with the server, and startup logs what it recovered ([#230](https://github.com/Crunging/Ludock/issues/230)) ([a833a5e](https://github.com/Crunging/Ludock/commit/a833a5ecc6fd99801776f0a3e15322887c188902))
* an interrupted restart of a crash-looping server no longer leaves it down, and recovery says only what it knows ([#273](https://github.com/Crunging/Ludock/issues/273)) ([8e3d288](https://github.com/Crunging/Ludock/commit/8e3d28824dca33a8e7bc2ea8c0cf3a8b4fe63d13))
* an old container started at the end of an update never puts back a new version that has settled ([#310](https://github.com/Crunging/Ludock/issues/310)) ([4288f29](https://github.com/Crunging/Ludock/commit/4288f29f824a89c220213aa60f687b9e0f54983f))
* an old container's start-up log says which container runs Ludock, and names only this update's ([#311](https://github.com/Crunging/Ludock/issues/311)) ([1680642](https://github.com/Crunging/Ludock/commit/1680642f36b59adea8049dd9e3ce34bd5b265c33))
* an old restore journal never leaves a server stopped, and an update leaves the server running or stopped as it was when Ludock stopped it ([#175](https://github.com/Crunging/Ludock/issues/175)) ([f2375f0](https://github.com/Crunging/Ludock/commit/f2375f017162b84293b3ae5e7eb29269ebb3a9b0))
* an outage that ended during a pause no longer shows until the next check ([#231](https://github.com/Crunging/Ludock/issues/231)) ([217db03](https://github.com/Crunging/Ludock/commit/217db032760a67ba39af53fea6a8d0d101f4d5b3))
* an unchecked server isn't shown as waiting for an update, setup takes a pasted code, and a few pages read plainer ([#314](https://github.com/Crunging/Ludock/issues/314)) ([3879f1a](https://github.com/Crunging/Ludock/commit/3879f1aee95785e25fb063f9ff50793215101e5a))
* an undo never deletes what another program wrote into the server's data ([#270](https://github.com/Crunging/Ludock/issues/270)) ([1faa35c](https://github.com/Crunging/Ludock/commit/1faa35c1c9cf657699e0d277916f3a3f4445a3a6))
* an undo stops moving data within about a second of the game being started outside Ludock ([#274](https://github.com/Crunging/Ludock/issues/274)) ([db3120e](https://github.com/Crunging/Ludock/commit/db3120ee5d5ecacb1cd82870ff0937a080bd2683))
* an unreadable console password says so instead of blaming the game, and Docker failures log as warnings with their cause ([#168](https://github.com/Crunging/Ludock/issues/168)) ([344e85b](https://github.com/Crunging/Ludock/commit/344e85bbaad1ce85f770e2c6fd8669a782d3c605))
* an update cut off before it had a helper no longer logs a helper's exit code ([#302](https://github.com/Crunging/Ludock/issues/302)) ([30c511c](https://github.com/Crunging/Ludock/commit/30c511c25ff24174d96b0166616079b895ac4ed5))
* an update recovered after a restart reports whether the server is really running ([#178](https://github.com/Crunging/Ludock/issues/178)) ([ad55d07](https://github.com/Crunging/Ludock/commit/ad55d07f65b9e180c228add458112e4f441e06f6))
* backup space on Settings and the Backups tab no longer counts archives that are gone ([#234](https://github.com/Crunging/Ludock/issues/234)) ([73bb364](https://github.com/Crunging/Ludock/commit/73bb364ffccd4f5ed61e66fbaaba1fdf6e5a908d))
* backups and restores warn players first, Needs attention forgets failures a retry fixed, and file actions no longer hang on Docker ([#136](https://github.com/Crunging/Ludock/issues/136)) ([19f2fbe](https://github.com/Crunging/Ludock/commit/19f2fbe90dcd16430e27b740ba68536c211283b2))
* backups keep ACLs, so a restore never gives a file's group access it didn't have ([#252](https://github.com/Crunging/Ludock/issues/252)) ([c7d665c](https://github.com/Crunging/Ludock/commit/c7d665caba22c5059fffd7b2753d264c7574b5e3))
* **backups:** release the second folder before reporting its work done ([#114](https://github.com/Crunging/Ludock/issues/114)) ([1e0dc46](https://github.com/Crunging/Ludock/commit/1e0dc46b55ede0a2613102e308095d2c04287ab4))
* clearer wording on several pages, and Ludock's data save works for large user IDs ([#251](https://github.com/Crunging/Ludock/issues/251)) ([e39442c](https://github.com/Crunging/Ludock/commit/e39442c4cd6a0be067636936cacf9f5493dc2121))
* Core Keeper, V Rising, Enshrouded, and 7 Days to Die back up only their worlds, and V Rising's and 7 Days to Die's consoles work as their images set them up ([#169](https://github.com/Crunging/Ludock/issues/169)) ([9b915e2](https://github.com/Crunging/Ludock/commit/9b915e21dd7eeadc76b033bc8e6bdd74a6a6987b))
* correct eight edge cases in files, alerts, schedules, players, and sign-in ([#100](https://github.com/Crunging/Ludock/issues/100)) ([e4e86e8](https://github.com/Crunging/Ludock/commit/e4e86e849c42400d39a6eb8d2604c869d5b5237b))
* deleting an account no longer deletes the schedules it set up ([#206](https://github.com/Crunging/Ludock/issues/206)) ([c0d4d6e](https://github.com/Crunging/Ludock/commit/c0d4d6ea7714448251352d4a0d4c053ad9c0f44c))
* drop or choose a whole mods folder in Files, asked once before anything is replaced ([#255](https://github.com/Crunging/Ludock/issues/255)) ([7d004c0](https://github.com/Crunging/Ludock/commit/7d004c003243b6c451974a3341d557b4f58d6573))
* failures say what went wrong in plain words, blame Docker when it was Docker, and a stopped server highlights Start ([#146](https://github.com/Crunging/Ludock/issues/146)) ([66efb6d](https://github.com/Crunging/Ludock/commit/66efb6da86dd25a509e0d681ae7543587ab56511))
* file changes aren't reported as failed when Docker confirms late, a quick click after a delete can't hit the wrong file, and a restore warns players who joined while it checked the backup ([#147](https://github.com/Crunging/Ludock/issues/147)) ([41f222c](https://github.com/Crunging/Ludock/commit/41f222ca16203d4496f69477e91e1048cfce0d60))
* files a game can't see stay out of reach on case-insensitive storage, and replaced files keep their ACLs ([#240](https://github.com/Crunging/Ludock/issues/240)) ([11bc3e9](https://github.com/Crunging/Ludock/commit/11bc3e965f4299eb5f36d417c6ffae87b9383512))
* Files can unpack, rename, and move folders on NFS and other storage without no-replace rename ([#298](https://github.com/Crunging/Ludock/issues/298)) ([8a76448](https://github.com/Crunging/Ludock/commit/8a76448a453e5730442ce4b29d38ec340d899b8d))
* Files no longer shows or changes Ludock's restore folders, and never empties a separately mounted folder ([#172](https://github.com/Crunging/Ludock/issues/172)) ([78956b0](https://github.com/Crunging/Ludock/commit/78956b0a10a10a8b3653af8cb718233ce914685d))
* first-week pages say what's going on and skip what an owner doesn't need ([#308](https://github.com/Crunging/Ludock/issues/308)) ([01f6670](https://github.com/Crunging/Ludock/commit/01f667080e37b4843939bb0bbcc966494cde6d1c))
* four notes on pages say what is true ([#219](https://github.com/Crunging/Ludock/issues/219)) ([8e391cc](https://github.com/Crunging/Ludock/commit/8e391cc7ed4c5abe82753746f9a945d2b556921c))
* games that can control the host join Ludock only when their owner labels them in ([#125](https://github.com/Crunging/Ludock/issues/125)) ([a4a3711](https://github.com/Crunging/Ludock/commit/a4a371107c2d712f93d43f971ff936f910eeac8e))
* keep servers usable when you change their containers in Docker; only a new game or data folder asks to confirm sharing ([#102](https://github.com/Crunging/Ludock/issues/102)) ([d2f6992](https://github.com/Crunging/Ludock/commit/d2f6992aab0f1e282a7aec0f1ceb045cdf22eb4e))
* Ludock calls each thing by one name, Activity can cancel waiting work, and the docs match the panel ([#150](https://github.com/Crunging/Ludock/issues/150)) ([788d485](https://github.com/Crunging/Ludock/commit/788d4856cac6793445bc5f8c77f6f6f72b3ead7c))
* Ludock never restarts a --rm server through Docker, and its pages say why just once ([#191](https://github.com/Crunging/Ludock/issues/191)) ([e5eced6](https://github.com/Crunging/Ludock/commit/e5eced6bd58217473008565f2f4e347c8bdd0391))
* Ludock never stops a server Docker deletes on stop, and a volume subfolder stays the only folder Ludock sees ([#173](https://github.com/Crunging/Ludock/issues/173)) ([70191d4](https://github.com/Crunging/Ludock/commit/70191d40acb66bcb080d89810c464c2c024eb1d2))
* Ludock recovers on its own when it or Docker restarts, and says once when a backup or restore was cut short ([#163](https://github.com/Crunging/Ludock/issues/163)) ([6e57d75](https://github.com/Crunging/Ludock/commit/6e57d75372bded645193b03f96db014f3169657a))
* Ludock stops within seconds, a stuck backup folder no longer holds up the panel, and backups on a lost disk can be removed ([#126](https://github.com/Crunging/Ludock/issues/126)) ([073630b](https://github.com/Crunging/Ludock/commit/073630b1333974a325750396a45f28c89979585a))
* Ludock survives Docker going away mid-restart, says plainly when Docker is the problem, and tabs never reload the page ([#135](https://github.com/Crunging/Ludock/issues/135)) ([f478ea8](https://github.com/Crunging/Ludock/commit/f478ea80fe66776b12779accc0a6035ecdb6829e))
* Ludock tells you when alerts aren't arriving, a test shows its result where you clicked, and each delivery shows the alert it sent ([#144](https://github.com/Crunging/Ludock/issues/144)) ([18f380e](https://github.com/Crunging/Ludock/commit/18f380e3757b69d6facfd41203227602a3d2b4b6))
* Ludock works on phones, in Firefox and Safari, and with a keyboard or screen reader ([#161](https://github.com/Crunging/Ludock/issues/161)) ([8d93518](https://github.com/Crunging/Ludock/commit/8d935189c0ddd18ce19e5edd39d78d9108bb4225))
* Monitoring says what is wrong, 7 Days to Die's password trouble is explained, a two-factor setup can be cancelled, and a refused change shows no list ([#171](https://github.com/Crunging/Ludock/issues/171)) ([f2c2e7a](https://github.com/Crunging/Ludock/commit/f2c2e7ad6e861b03548721d18de68562adcbfa45))
* moving a renamed server's schedules no longer deletes one that backs up first ([#248](https://github.com/Crunging/Ludock/issues/248)) ([f279986](https://github.com/Crunging/Ludock/commit/f2799866afe87de81d7f227d4a211696cbcda875))
* Needs attention and undo recovery decide from saved facts, not from the wording of a message ([#279](https://github.com/Crunging/Ludock/issues/279)) ([e6b7c0c](https://github.com/Crunging/Ludock/commit/e6b7c0c2231fded934cb1477e585e4187b055485))
* on a phone the server list comes before the setup list, which folds to one line ([#237](https://github.com/Crunging/Ludock/issues/237)) ([6c7f215](https://github.com/Crunging/Ludock/commit/6c7f21569a1c2c2508b01286b81dd2b2ad2c4216))
* one file or folder restores on storage without no-replace rename, such as NFS ([#280](https://github.com/Crunging/Ludock/issues/280)) ([075cd1f](https://github.com/Crunging/Ludock/commit/075cd1f4405761e78a043b00d507b4afd311db6a))
* one game whose status check hangs no longer slows every other server's player count and health ([#157](https://github.com/Crunging/Ludock/issues/157)) ([45e2d0e](https://github.com/Crunging/Ludock/commit/45e2d0e8c4b7216fe20d2b25545e616ccacfd7a9))
* one outage, one alert, even if monitoring settings are saved while it lasts ([#246](https://github.com/Crunging/Ludock/issues/246)) ([a94da2e](https://github.com/Crunging/Ludock/commit/a94da2e41a982a26ebdf5d67275d2a8926fc59d7))
* other sessions say Sign out like Activity does, Settings says why backups are off, and two alerts that could never send are gone ([#167](https://github.com/Crunging/Ludock/issues/167)) ([64a4ad7](https://github.com/Crunging/Ludock/commit/64a4ad79ae2b3aebc361b5797d4e0d65bd9b2d0a))
* pages tell the owner the truth about backups, interruptions, and a blocked undo, and how to get out ([#257](https://github.com/Crunging/Ludock/issues/257)) ([168b591](https://github.com/Crunging/Ludock/commit/168b591ede6f48abece04300eed4bbd8837a7e3e))
* players warned before an undone restore hear why the server restarts ([#194](https://github.com/Crunging/Ludock/issues/194)) ([3ad9850](https://github.com/Crunging/Ludock/commit/3ad985065b7f1728c4b8b6f2fd01ba1c5396eaf6))
* players warned of a scheduled restart hear it's off when the schedule is paused, changed, or deleted, or the run is dropped ([#131](https://github.com/Crunging/Ludock/issues/131)) ([d839273](https://github.com/Crunging/Ludock/commit/d839273ad6df134cfe034f2907edea94e475d5d8))
* removing someone's access stops their request even if it is already on its way ([#223](https://github.com/Crunging/Ludock/issues/223)) ([2902c18](https://github.com/Crunging/Ludock/commit/2902c183881d69e40235d2ac475e7faac4137f73))
* renaming a game label to its other spelling no longer pauses sharing or orphans backups ([#204](https://github.com/Crunging/Ludock/issues/204)) ([8534407](https://github.com/Crunging/Ludock/commit/85344072710bb43b0a8834b9c446f589ebce8612))
* restore progress in plain words, a space before the time of a failed backup, clearer test results for email and webhooks, and downloaded backups named after their server and date ([#164](https://github.com/Crunging/Ludock/issues/164)) ([6eced73](https://github.com/Crunging/Ludock/commit/6eced732a05e75df415fc9f925129d227f44d2de))
* retention never deletes the last backup that can still be restored ([#176](https://github.com/Crunging/Ludock/issues/176)) ([dc6bfc2](https://github.com/Crunging/Ludock/commit/dc6bfc25ee72af9a64e56785c95c02154e0f16aa))
* safety backups no longer push out the backups you made yourself ([#217](https://github.com/Crunging/Ludock/issues/217)) ([c1eaa62](https://github.com/Crunging/Ludock/commit/c1eaa6224f6b9d9f036b4ec6745dfc10bf092a28))
* second copies are dated as their backup, and the docs cover removing Ludock, restoring by hand with owners kept, and the volume docker compose down -v leaves ([#165](https://github.com/Crunging/Ludock/issues/165)) ([bd9abf5](https://github.com/Crunging/Ludock/commit/bd9abf57400f83b96992e17e80a723e7bc1d2ff9))
* servers Compose made are called by their service's name, told apart by project where needed ([#307](https://github.com/Crunging/Ludock/issues/307)) ([2b2d7d4](https://github.com/Crunging/Ludock/commit/2b2d7d4ac4d628fec358d38452abc0ed95b60edd))
* Settings → Advanced saves each section the same way, and a save never loses what is typed ([#236](https://github.com/Crunging/Ludock/issues/236)) ([4154e0c](https://github.com/Crunging/Ludock/commit/4154e0c50f795ee444d713f223e70ebf18d34e48))
* show a finished operation on the server list right away ([#94](https://github.com/Crunging/Ludock/issues/94)) ([48d09a6](https://github.com/Crunging/Ludock/commit/48d09a688417d944013482bc27096aa37d2648d3))
* sign-in limits hold behind any proxy, stalled requests can't pin connections, and Files never offers Ludock's own data ([#160](https://github.com/Crunging/Ludock/issues/160)) ([2136144](https://github.com/Crunging/Ludock/commit/2136144a824182962d485af383e0a5c277de4ccd))
* stop backups, schedules, and restores failing quietly, and cut idle Docker work ([#83](https://github.com/Crunging/Ludock/issues/83)) ([63904db](https://github.com/Crunging/Ludock/commit/63904dba775d3f9058fe9419d4601a9636efd16b))
* stopping Ludock while it reconciles interrupted work no longer races the work queue's start ([#306](https://github.com/Crunging/Ludock/issues/306)) ([cb8bbf5](https://github.com/Crunging/Ludock/commit/cb8bbf59335638c60cdd62ae93cb92b77eaef748))
* Terraria, Factorio, Valheim, and Palworld servers back up, talk, and list players on their real images ([#138](https://github.com/Crunging/Ludock/issues/138)) ([6bcff88](https://github.com/Crunging/Ludock/commit/6bcff88da6e99aaad27a7cc71a15685263830f2f))
* the backend checks every upload folder approval against its own file list, not just the count ([#267](https://github.com/Crunging/Ludock/issues/267)) ([f995576](https://github.com/Crunging/Ludock/commit/f995576596ca645162813c545321ffa94dea5e8d))
* the backup Delete button carries one action, and two notes say what really happens ([#212](https://github.com/Crunging/Ludock/issues/212)) ([4e5b613](https://github.com/Crunging/Ludock/commit/4e5b613645ad30e9ce16423d8d1814c1dd2a3227))
* the server list, Needs attention, and new schedules stop offering work that an unfinished restore would refuse ([#268](https://github.com/Crunging/Ludock/issues/268)) ([5a6b460](https://github.com/Crunging/Ludock/commit/5a6b46092feaabaf12b4cc782a9b7d611bbee24a))
* two schedules at the same time are pointed out before saving, and the second waits for the game to come back ([#282](https://github.com/Crunging/Ludock/issues/282)) ([5103f3f](https://github.com/Crunging/Ludock/commit/5103f3fcdb919ab7a1fe4ae3dd0e65224489c963))
* Undo no longer leaves a server down when its game recreated a folder after an interrupted restore ([#253](https://github.com/Crunging/Ludock/issues/253)) ([e7c0017](https://github.com/Crunging/Ludock/commit/e7c0017f829d4952f473c56ce4d9d545ee1c63e7))
* Update Ludock's alerts and notices send owners to Settings and say only what is true ([#291](https://github.com/Crunging/Ludock/issues/291)) ([31d7991](https://github.com/Crunging/Ludock/commit/31d79915b85d081b8801f5529570201901caeee0))
* update recovery no longer calls a crash-looping or missing server finished ([#199](https://github.com/Crunging/Ludock/issues/199)) ([35d12aa](https://github.com/Crunging/Ludock/commit/35d12aaee66adeeb9a5d2894e99ffd444d924e5b))
* updates and docker compose pull no longer lose the server, and backups save the world first ([#133](https://github.com/Crunging/Ludock/issues/133)) ([ea40b12](https://github.com/Crunging/Ludock/commit/ea40b127a7026c8035b1d4f34bb0375ae9b28a23))
* uploaded and unpacked files get exactly what the folder's default ACL gives, not more or less ([#254](https://github.com/Crunging/Ludock/issues/254)) ([bf54810](https://github.com/Crunging/Ludock/commit/bf548107c832c313a828a8722d59811ddb24c059))
* uploading a folder of mods takes seconds, not minutes ([#226](https://github.com/Crunging/Ludock/issues/226)) ([495a4ef](https://github.com/Crunging/Ludock/commit/495a4ef3c5578a5b97167c3e21d7b8ebcebc8339))
* use plain, accurate wording across pages, messages, and docs ([#162](https://github.com/Crunging/Ludock/issues/162)) ([c4fc69f](https://github.com/Crunging/Ludock/commit/c4fc69f048af5b44bf1411aaecc9733a671b16f2))
* warnings before a restart, backup or update go to whoever is online when it runs, and backup settings no longer hang on a stuck folder or Docker ([#141](https://github.com/Crunging/Ludock/issues/141)) ([68459f0](https://github.com/Crunging/Ludock/commit/68459f0689cd39851bc2d2fb8923011db21d828b))
* what the game wrote during an undone restore is kept in a folder its owner can open ([#220](https://github.com/Crunging/Ludock/issues/220)) ([b54a607](https://github.com/Crunging/Ludock/commit/b54a6078563b03b496122e3a3460a0fe7eb4d9c3))
* whether a restore couldn't check its set-aside folder is saved as a field, not read back from Activity's wording ([#261](https://github.com/Crunging/Ludock/issues/261)) ([8de7db4](https://github.com/Crunging/Ludock/commit/8de7db4ec60b025d9d2e3c139cc1c3a76c0089f8))
* work Ludock finished after its own restart ends as succeeded, not "Interrupted" ([#266](https://github.com/Crunging/Ludock/issues/266)) ([ae595b2](https://github.com/Crunging/Ludock/commit/ae595b2fc74138d68ba5f5a47aca22b389280830))
* work that can't finish an earlier restore refuses before stopping the game ([#202](https://github.com/Crunging/Ludock/issues/202)) ([685da51](https://github.com/Crunging/Ludock/commit/685da515a19c0e5ed57491aa500d4af96d8d4c61))


### Performance Improvements

* finishing an operation finds its Activity entry by operation, not by reading the server's whole history ([#130](https://github.com/Crunging/Ludock/issues/130)) ([7bb3fd2](https://github.com/Crunging/Ludock/commit/7bb3fd28d9eb92789aca3a6acecbdbdd48ceda58))
* Ludock stops writing to your game's log, file previews no longer start a container each, and a server page reads only its own server ([#139](https://github.com/Crunging/Ludock/issues/139)) ([a7c3d8d](https://github.com/Crunging/Ludock/commit/a7c3d8d9595d0ffe2a74f88a9331e0f31bc7f75b))


### Code Refactoring

* an undo names its set-aside folder whenever live data moved, instead of reading the folder back ([#287](https://github.com/Crunging/Ludock/issues/287)) ([f64edf6](https://github.com/Crunging/Ludock/commit/f64edf679019e8fc64dfd08a80b52fa7bad4fe8f))
* an undo renames by one rule, without probing for no-replace rename first ([#288](https://github.com/Crunging/Ludock/issues/288)) ([b1ed6f9](https://github.com/Crunging/Ludock/commit/b1ed6f951d0a11a074694165839a9c7baf0907fb))
* background server reads and admin-only renders each confirm access through one helper ([#134](https://github.com/Crunging/Ludock/issues/134)) ([bd3dca0](https://github.com/Crunging/Ludock/commit/bd3dca0a254c5dfe15550c55aee7a01d581658bc))
* background work, settings reads, and the yielding lock acquire each have one shared helper ([#132](https://github.com/Crunging/Ludock/issues/132)) ([86e0a35](https://github.com/Crunging/Ludock/commit/86e0a3573546127c4aea8b47f9d6f4c81adfa045))
* drop the restore journal fields and the returned copy that no release writes ([#297](https://github.com/Crunging/Ludock/issues/297)) ([f2a4022](https://github.com/Crunging/Ludock/commit/f2a40229e8b9b54b85afecd398d7bd05b90d1f4d))
* file and backup helper commands share one implementation, so the next fix to an uncertain command lands once ([#143](https://github.com/Crunging/Ludock/issues/143)) ([07ea5dd](https://github.com/Crunging/Ludock/commit/07ea5ddabff426f7a6cbf87759d8e380ab7336bd))
* Ludock reads its own container one way, so Files can't offer its folders before it has looked ([#193](https://github.com/Crunging/Ludock/issues/193)) ([6782798](https://github.com/Crunging/Ludock/commit/6782798c8326b1e002dfe1c62b8c10a3844bc289))
* mounts are read one way, and three tests stop depending on timing ([#225](https://github.com/Crunging/Ludock/issues/225)) ([1b2b7a4](https://github.com/Crunging/Ludock/commit/1b2b7a4986d63115551ea9d0e94338ce53aa3197))
* one hardened helper setup, one partial-file name, and no dead database parameters ([#186](https://github.com/Crunging/Ludock/issues/186)) ([b9cb230](https://github.com/Crunging/Ludock/commit/b9cb23056a3af34c6398db93d9d33efa6bbc3523))
* operation results have named fields the compiler checks ([#239](https://github.com/Crunging/Ludock/issues/239)) ([fc4b1c7](https://github.com/Crunging/Ludock/commit/fc4b1c7b84bdee9e4a51bcff4c670f4d12c28151))
* remove the restore helper's rollbackClean, which no undo asks for since [#270](https://github.com/Crunging/Ludock/issues/270) ([#277](https://github.com/Crunging/Ludock/issues/277)) ([90e9626](https://github.com/Crunging/Ludock/commit/90e9626a3a9dd09314adb4994ee3dc9c9bf38fca))
* remove unreachable code and unused styles ([#129](https://github.com/Crunging/Ludock/issues/129)) ([2699841](https://github.com/Crunging/Ludock/commit/269984157d6ff978a6b406b7512cee79635b076d))
* restore and recovery state each rule once, so 1.0's riskiest code reads plainly ([#182](https://github.com/Crunging/Ludock/issues/182)) ([58252b2](https://github.com/Crunging/Ludock/commit/58252b2da278d4377c0204fc46b24f76b3a46a88))
* the --rm rule and job kinds are each written once, so new work can't miss a copy ([#190](https://github.com/Crunging/Ludock/issues/190)) ([f241f39](https://github.com/Crunging/Ludock/commit/f241f39169ac74201cac327b8bb3b56f22318124))
* the fake Docker daemon runs helpers one way for every test, in files by topic ([#184](https://github.com/Crunging/Ludock/issues/184)) ([31532f7](https://github.com/Crunging/Ludock/commit/31532f7fe84947659101978ddb79fcea79da7451))
* the restore reads as its phases in order, with tests pinning each journal save and check ([#149](https://github.com/Crunging/Ludock/issues/149)) ([87eb89e](https://github.com/Crunging/Ludock/commit/87eb89ec4cbe2549cb5baf4ef834010b1475c392))
* the restore-extract format and the stalled-folder guard each have one definition ([#137](https://github.com/Crunging/Ludock/issues/137)) ([dffb687](https://github.com/Crunging/Ludock/commit/dffb6873251dac82bf5124b302c735b5e5e26170))
* the server table says "sharing paused" and stops keeping a binding history nobody reads ([#140](https://github.com/Crunging/Ludock/issues/140)) ([3802d5d](https://github.com/Crunging/Ludock/commit/3802d5d2802473a541ccb52ce409c0cc2da5484a))
* the update plan and Docker events name the helper label by its constant ([#192](https://github.com/Crunging/Ludock/issues/192)) ([341645d](https://github.com/Crunging/Ludock/commit/341645dd989209cbe09a23a8f08d9ddd527fef47))
* writing and unpacking an archive read as their steps ([#228](https://github.com/Crunging/Ludock/issues/228)) ([488b3f5](https://github.com/Crunging/Ludock/commit/488b3f597759e81bd3909069e3b0a9062fe1b359))

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
