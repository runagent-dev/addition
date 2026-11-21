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
| `ADDITION_ALLOW_WGET` | unset | `1` lets the installer use wget when curl is missing (less safe, see below) |

Re-running the script upgrades in place. The installer verifies one thing: the sha256 of the
archive against the `checksums.txt` published next to it. Both files come from the same release
origin, so with `ADDITION_BASE_URL` pointing at a mirror, the mirror has to be trusted. It also
refuses an archive whose members are not plain files with exactly one `addition`, and caps the
download at 100 MiB and the binary at 200 MiB. It does not verify provenance. A release has seven uploaded assets (four `tar.gz`, two `zip`
and `checksums.txt`), and each carries a GitHub artifact attestation; the source archives GitHub
adds to every release are not attested. Check an asset separately:

```bash
gh attestation verify addition_linux_amd64.tar.gz \
  --repo runagent-dev/addition \
  --signer-workflow runagent-dev/addition/.github/workflows/release.yml
```

The installer needs curl, tar, gzip, and sha256sum or shasum. Downloads use curl only, which is
held to HTTPS across redirects. Without curl it stops with `no-downloader`; set `ADDITION_ALLOW_WGET=1`
to use wget instead. That is less safe: wget may follow an https to http redirect.

Keep the install directory writable by you alone: the installer checks the destination and then
replaces it, and does not defend against another user changing the directory in between. How releases are cut and verified: [docs/RELEASING.md](docs/RELEASING.md).

Other ways in:

- **Go:** `go install github.com/runagent-dev/addition/cmd/addition@latest`
- **Windows:** download the `.zip` for your CPU from the
  [releases page](https://github.com/runagent-dev/addition/releases) and put `addition.exe` on your `PATH`.

## For agents

Install it (above), then run `addition guide`: the manual for agents is built into the binary,
matches its version and works offline. [`llms.txt`](llms.txt) indexes the same pages for crawlers.

## Quick start

```bash
addition init                                           # create the vault and its master key
addition doctor                                         # what is wrong, and the one command to run next
addition client add google --file client_secret.json    # your own OAuth client; shared by every Google app
addition connect gmail --as work                        # one connection per app
addition connect google-calendar --as work
addition accounts list
addition accounts check --live                          # does each token still work?
addition doctor --live                                  # also probe each app's API once
addition call google-calendar -a work GET /calendar/v3/calendars/primary/events
addition call gmail -a work GET /gmail/v1/users/me/messages --query q=newer_than:7d
addition providers                                      # apps addition knows
addition tools                                          # curated tools, as JSON
addition version
```

No desktop keyring (a Linux server without a Secret Service)? Plain `addition init` fails with
`keychain-unavailable`. Keep the master key in a file instead, `addition init --key-file` (it goes to
`$ADDITION_HOME/master.key`, mode 0600), or export `ADDITION_MASTER_KEY` (64 hex characters) and run
`addition init` as usual; the variable must then stay set for every later command.

`addition doctor` prints one JSON report: `ok`, a list of `checks` (vault, master key,
OAuth client, each account) and `next`, the single command to run now. It exits 3 when you need to act.
Run it first after installing, and whenever something fails. doctor opens the vault like any other command
(it may apply a pending schema upgrade) and makes no network request unless you pass `--live`.

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
