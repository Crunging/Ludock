# Security policy

## Reporting a vulnerability

Please don't open a public issue for a suspected vulnerability. Use
[GitHub's private vulnerability reporting](https://github.com/Crunging/Ludock/security/advisories/new)
and include the affected version or commit, steps to reproduce, what you
expected and what happened, and any known impact.

Security fixes go into the latest release. Nightly images are development
builds and aren't supported.

## Deploying safely

- **The Docker socket is control of the host.** Anyone who controls Ludock can
  control every container and the host itself, even with the socket mounted
  read-only. Run one Ludock per trusted Docker host, on a trusted network.
- **Remote access:** reach Ludock from elsewhere only through an HTTPS reverse
  proxy, and trust only that proxy's own address in **Settings → Advanced →
  Reverse proxy**: a trusted address can claim to be any visitor. See
  [Remote access](./docs/REFERENCE.md#remote-access).
- **Sign-in:** passwords need at least 15 characters, and repeated failures are
  limited per address. Ask administrators to turn on two-factor sign-in in
  **Account**.
- **Invite and reset links** set a password for whoever opens them first. Send
  them privately; they work once and expire after 24 hours.
- **Access:** administrators use every server. Members see only the servers
  shared with them, at the access chosen on each server's **Sharing** tab, and
  never the Shell, updates, or backup downloads.
- **Discovered containers:** containers on supported game images are added by
  themselves unless labeled `ludock.enable: "false"`. Recognizing an image
  doesn't prove where it came from. Containers that could control the host
  are added only with their owner's `ludock.enable: "true"` label; see
  [Servers that can control the host](./docs/GUIDE.md#servers-that-can-control-the-host).
- **Sharing a server's console or files hands them over.** Members never get
  the Shell, but game console commands and changed files can run code inside
  the container, and for a container that can control the host, the host.
- **The backup folders** hold game backups, which can include configuration
  secrets, and Ludock's own data (`ludock-data-*.tar`): password hashes,
  two-factor secrets, sessions, notification tokens, and the key that protects
  them. Anyone who can read the backup folder or the second copy can read
  these, so keep them as private as Ludock's data volume.
- **File access:** approve narrow folders with `ludock.files` where you can, and
  keep registry and webhook credentials out of game folders. A game folder that
  holds, or is inside, Ludock's own data or backup folders is never offered.
