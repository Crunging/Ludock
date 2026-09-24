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
trusted Docker host and use an HTTPS reverse proxy for remote access. Recognized
game images are enrolled automatically unless opted out; image recognition is
not proof of provenance. Administrators see eligible servers immediately, while
other users require explicit per-server/action grants. Approve narrow Compose
and backup roots, and keep registry and webhook credentials out of exposed game
file roots. See [Operations](./docs/OPERATIONS.md#deployment) for deployment
guidance.
