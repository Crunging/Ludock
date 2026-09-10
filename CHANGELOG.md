# Changelog

## [0.3.0](https://github.com/Crunging/Ludock/compare/v0.2.0...v0.3.0) (2026-09-10)


### ⚠ BREAKING CHANGES

* **v2:** rewrite Ludock with scoped access and durable server operations ([#3](https://github.com/Crunging/Ludock/issues/3))

### Features

* add game discovery and structured logging ([b491e5e](https://github.com/Crunging/Ludock/commit/b491e5eb0907ae575610e3ba52f587f367d31184))
* add secure multi-user game server management ([5fe9c01](https://github.com/Crunging/Ludock/commit/5fe9c01efd7dee1e421e54bff54f98aa140973b3))
* **ci:** add release automation and backend linting ([57738f0](https://github.com/Crunging/Ludock/commit/57738f0417139610582eacd9e0d3994ab8913196))
* **deploy:** use published images in Compose ([174bf19](https://github.com/Crunging/Ludock/commit/174bf191a28cf51b4fb177771e34398d5e41b913))
* **logs:** add Docker and Ludock log viewers ([4fe0565](https://github.com/Crunging/Ludock/commit/4fe05654cd407c300c4cee08bf3387853734e97d))
* **security:** harden authentication and request handling ([1498947](https://github.com/Crunging/Ludock/commit/1498947612b15dfb77de13c9c8595463fe17b95e))
* **v2:** rewrite Ludock with scoped access and durable server operations ([#3](https://github.com/Crunging/Ludock/issues/3)) ([df084a9](https://github.com/Crunging/Ludock/commit/df084a9f94daab660a5a1131225dcd84d6cf5abc))


### Bug Fixes

* **auth:** handle proxied request origins safely ([fb3ffcd](https://github.com/Crunging/Ludock/commit/fb3ffcd1a03d15604cc3e76f09f7f994b9166655))
* **auth:** tolerate malformed session cookies ([a78a6fc](https://github.com/Crunging/Ludock/commit/a78a6fc474db035cfb6893487733c90a353cd192))
* **console:** write commands directly to container stdin ([3c7b96c](https://github.com/Crunging/Ludock/commit/3c7b96c04aad480b5a33d2d2b8238908e03ec156))
* **frontend:** retry auth status checks and show connection errors ([656f95d](https://github.com/Crunging/Ludock/commit/656f95d560f0c60011b0daa79d88d8d49533fb17))
* **proxy:** use forwarded host metadata for origin checks ([bfdf10b](https://github.com/Crunging/Ludock/commit/bfdf10b68982c99ef3aabc0a7ac27c8e55d2c35d))
* **security:** harden authentication and request handling ([#1](https://github.com/Crunging/Ludock/issues/1)) ([88e0981](https://github.com/Crunging/Ludock/commit/88e0981768dad5f72f87d9f415ab36098b2e89c9))
