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
- Ludock saves its own data, including password hashes, two-factor secrets,
  sessions, notification tokens, and the identity key that protects them, as
  `ludock-data-*.tar` files in the backup folder and the second folder.
  Anyone who can read either folder can read Ludock's data: keep them as
  private as Ludock's data volume. The panel never lists or offers these
  files.
- Behind a reverse proxy, trust only the proxy's own address in
  **Settings → Advanced**: a trusted address can claim to be any visitor.

See [Remote access](./docs/OPERATIONS.md#remote-access) for proxy and network
settings.
