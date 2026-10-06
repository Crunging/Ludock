# Security policy

## Supported releases

Security fixes go into the latest published release. Nightly images are
development builds and aren't supported.

## Reporting a vulnerability

Please don't open a public issue for a suspected vulnerability. Use
[GitHub's private vulnerability reporting](https://github.com/Crunging/Ludock/security/advisories/new)
and include:

- The affected version or commit
- Steps to reproduce
- What you expected and what happened
- Any known impact or suggested mitigation

## Deploying safely

Access to the Docker socket gives control of the whole host. Run Ludock only on a
trusted Docker host, and use an HTTPS reverse proxy for remote access.

- Supported game images are added automatically unless opted out. Recognizing
  an image doesn't prove where it came from.
- Administrators see every eligible server right away. Other users see only the
  servers and actions shared with them.
- Approve narrow Compose and backup folders, and keep registry and webhook
  credentials out of game file folders.

See [Deployment](./docs/OPERATIONS.md#deployment) for proxy and network settings.
