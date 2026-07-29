# Security policy

## Supported releases

Security fixes are provided for the latest published release. Nightly images
are development builds and are not supported.

## Reporting a vulnerability

Please do not open a public issue for a suspected vulnerability. Use
[GitHub's private vulnerability reporting](https://github.com/Crunging/ludock/security/advisories/new)
and include:

- The affected version or commit
- Reproduction steps
- The expected and observed behavior
- Any known impact or suggested mitigation

## Deployment boundary

Docker socket access grants host-level power. Only deploy this panel on a
trusted Docker host, opt in containers deliberately, and use an HTTPS reverse
proxy for remote access. See the [README](./README.md#security) for deployment
guidance.
