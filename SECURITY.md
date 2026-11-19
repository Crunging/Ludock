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

- Supported game images are added automatically unless labeled
  `ludock.enable: "false"`. Recognizing an image doesn't prove where it came
  from.
- Ask administrators to turn on two-factor sign-in in **Account**; an
  administrator account controls the Docker socket.
- Send invite and password reset links privately: anyone who opens one before
  the user does can set the password. They work once and expire after a day.
- Administrators see every managed server. Members see only the servers shared
  with them, with the access chosen on each server's Sharing tab.
- Approve narrow backup folders, and keep registry and webhook credentials out
  of game file folders.

See [Deployment](./docs/OPERATIONS.md#deployment) for proxy and network settings.
