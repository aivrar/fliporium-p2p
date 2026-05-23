# Security Policy

## Reporting a vulnerability

Please report security issues **privately** — email **chris@fliporium.com** or
use the contact form at <https://fliporium.com/contact>. Do **not** open a public
GitHub issue for a vulnerability.

Include what you found, how to reproduce it, and the impact. We'll acknowledge
your report as quickly as we can and keep you updated on a fix. Responsible
disclosure is appreciated — please give us a chance to ship a fix before
publishing details.

## Supported versions

The official build at <https://fliporium.com> is the supported version. Security
fixes ship there (and to this repository) as new releases.

## Security model (what protects you)

- **End-to-end encryption.** Every room has a 32-byte NaCl secretbox key carried
  only in the invite link's URL fragment, which a browser never sends to any
  server. Message bodies and the offline backlog are sealed with it.
- **Authenticated peers.** Each install has an Ed25519 keypair; the routing id is
  its fingerprint, and a signed challenge — bound to *both* sides' nonces — proves
  key ownership, preventing impersonation and relay/MITM.
- **The coordination server sees only ciphertext + metadata** (room ids, chosen
  display names, who's online) — never message or file content. There are no
  accounts, so none of that metadata ties to a real identity.

## By design — not vulnerabilities

These are inherent to the model, not bugs:

- **Anyone with a room's invite link can read that room.** The link *is* the key.
  Treat invite links like passwords; to remove someone, create a new room.
- **Any member of a room can read everything in it** — the room key is shared.
- **Blocking is per-identity.** A blocked peer can generate a fresh identity;
  with no accounts there's no permanent, enforceable ban.
- **Running a modified or forked build is on you.** Only the official build from
  <https://fliporium.com> is vetted; don't run look-alike binaries.

## Scope

In scope: the desktop app, the signaling server (`flipsignal`), and the public
`flipstats` service. Out of scope: social engineering, physical access to an
unlocked device, denial-of-service that requires abusive traffic volumes, and
anything that requires a malicious fork the victim chose to run.
