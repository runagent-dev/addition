# Addition

`addition` is a local-first CLI that gives your coding agents authenticated access
to your accounts — several Gmail inboxes, Drives and calendars, and later
GitHub, Slack and Discord — from any harness, on any machine.

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/runagent-dev/addition/main/install.sh | sh
```

The script never asks questions and never uses `sudo`. It installs one binary to
`~/.local/bin`, prints progress on stderr, and prints one JSON line on stdout:
`{"installed":true,"version":"…","path":"…","on_path":true,"next":"addition init"}`. It does not
edit your shell rc files; when the directory is not on your `PATH` it prints the line to add.

| Variable | Default | Meaning |
|---|---|---|
| `ADDITION_VERSION` | latest release | `v0.1.0` or `0.1.0` pins a release |
| `ADDITION_INSTALL_DIR` | `$HOME/.local/bin` | where the binary goes; created if missing |
| `ADDITION_BASE_URL` | `https://github.com/runagent-dev/addition/releases` | releases URL, for mirrors and tests |

Re-running the script upgrades in place. Every download is checked against the release's
`checksums.txt` (sha256) and refused on a mismatch, and every release file carries a GitHub
artifact attestation:

```bash
gh attestation verify addition_linux_amd64.tar.gz --repo runagent-dev/addition
```

Other ways in:

- **Go:** `go install github.com/runagent-dev/addition/cmd/addition@latest`
- **Windows:** download the `.zip` for your CPU from the
  [releases page](https://github.com/runagent-dev/addition/releases) and put `addition.exe` on your `PATH`.

## Quick start

```bash
addition init                                           # create the vault and its master key
addition client add google --file client_secret.json    # your own OAuth client; shared by every Google app
addition connect gmail --as work                        # one connection per app
addition connect google-calendar --as work
addition accounts list
addition accounts check --live                          # does each token still work?
addition call google-calendar -a work GET /calendar/v3/calendars/primary/events
addition call gmail -a work GET /gmail/v1/users/me/messages --query q=newer_than:7d
addition providers                                      # apps addition knows
addition tools                                          # curated tools, as JSON
addition version
```

Headless or over SSH: `addition connect gmail --as work --no-browser`, then finish with
`addition connect --complete '<url the browser landed on>'`. Run `addition <command> --help` for details.

### Coming

These friendly commands are planned and do not exist yet; `addition call` reaches the same APIs today.

```bash
addition gmail search-messages -a work --query "has:attachment newer_than:7d"
addition gmail get-attachment  -a work --message <id> --attachment <id> -o ./invoice.pdf
addition drive upload          -a work --file ./invoice.pdf --folder "Taxes/2026"
```

## Why addition

- **Local only.** Credentials live in one encrypted SQLite file on your machine.
  No server, no account, no telemetry.
- **One binary.** Static Go build for Linux, macOS and Windows.
- **One connection per app.** Gmail, Drive, Calendar, Chat and Docs are separate
  connections: each asks only for its own scopes and can be removed alone, and
  its token goes only to its own API hosts and routes. Google revokes a whole
  grant at once, so `addition accounts remove <app>:<alias> --revoke` refuses when
  other apps of the same Google user would stop working, unless you add
  `--and-siblings` (they are then marked `needs-reauth`).
- **Built for agents.** Tool commands print JSON on stdout, errors are
  structured JSON on stderr, exit codes are stable, and files go in and out.

> **Status:** early development. Connecting accounts and `addition call` work today; the
> curated tool commands arrive with `v0.1.0` ("Google for my agents"). See
> [`docs/cli-plan.md`](docs/cli-plan.md) for the plan.

## Development

```bash
go test ./...
go run ./cmd/addition version
```

The earlier TypeScript/Python SDK lives on the `archive/sdk-v0.1` branch.

## License

Apache License 2.0. Copyright 2026 RunAgent Technology Limited. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
