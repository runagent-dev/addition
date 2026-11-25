# @addition/cli

`npx @addition/cli <command>` runs [addition](https://github.com/runagent-dev/addition): a local-first CLI that gives
coding agents authenticated access to your accounts.

This package holds no program. On first use it downloads the release binary for your platform from
GitHub, checks it against the release's `checksums.txt`, keeps it under `$ADDITION_HOME/runtime/versions/<version>`
and runs it with your arguments, standard input and exit code. After that it makes no network request.

```bash
npx @addition/cli init
npx @addition/cli connect clickup --as work --token-stdin
npm i -g @addition/cli        # puts `addition` on PATH for agents
```

- **Needs** Node 20.11 or newer. The binary itself needs nothing.
- **Platforms** linux, macOS and Windows on x64 and arm64.
- **Version** the package version is the release it fetches (`@addition/cli@0.2.0` runs `addition` 0.2.0).
- **Mirrors and tests** `ADDITION_BASE_URL` replaces `https://github.com/runagent-dev/addition/releases`; it must be https,
  or http on 127.0.0.1 or localhost.
- **Errors** are the same JSON envelope the binary prints, on stderr, with exit code 1 when the download or
  verification fails: `checksum-mismatch`, `asset-not-listed`, `release-not-found`, `version-mismatch`,
  `bad-archive`, `unsupported-platform`, `insecure-base-url`, and a few more.

Licence: Apache-2.0.
