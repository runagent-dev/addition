# Security policy

Addition keeps OAuth tokens for your accounts in a local encrypted vault, so security reports are taken seriously.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private reporting: **Security → Report a vulnerability** on
<https://github.com/runagent-dev/addition/security/advisories/new>.

Include the version (`addition version`), your OS, and the smallest steps that reproduce the problem. Do not include real tokens or client secrets.

## What counts

Anything that breaks a promise in the README or [CLAUDE.md](CLAUDE.md) §2 and §6, for example:

- a token or client secret reaching stdout, stderr, logs, errors, `-o` files, or a provider echo;
- an access token sent to a host or route outside the connected app's allow-list, or a redirect being followed;
- reading or writing the vault, the master key or the data directory without the owner's permission;
- a weakness in the installer (`install.sh`) or the release artifacts (checksums, attestations);
- a way to make one connected app act on another app's or account's credentials.

## Supported versions

Only the latest release receives fixes while the project is pre-1.0.

## What to expect

You will get an acknowledgement and a first assessment as soon as a maintainer can look; fixes ship in a normal release and are credited to you unless you prefer otherwise.
