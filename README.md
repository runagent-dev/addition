# Addition

`addn` is a local-first CLI that gives your coding agents authenticated access
to your accounts — several Gmail inboxes, Drives and calendars, and later
GitHub, Slack and Discord — from any harness, on any machine.

```bash
addn client add google --file client_secret.json   # once; shared by every Google app
addn connect gmail --as work
addn connect google-drive --as work
addn connect google-calendar --as work
addn connect google-chat --as work
addn gmail search-messages -a work --query "has:attachment newer_than:7d"
addn gmail get-attachment  -a work --message <id> --attachment <id> -o ./invoice.pdf
addn drive upload          -a work --file ./invoice.pdf --folder "Taxes/2026"
addn call google-calendar -a work GET /calendar/v3/calendars/primary/events
addn call google-chat -a work GET /v1/spaces
addn accounts check --live
```

- **Local only.** Credentials live in one encrypted SQLite file on your machine.
  No server, no account, no telemetry.
- **One binary.** Static Go build for Linux, macOS and Windows.
- **One connection per app.** Gmail, Drive, Calendar, Chat and Docs are separate
  connections: each asks only for its own scopes and can be removed alone, and
  its token goes only to its own API hosts and routes. Google revokes a whole
  grant at once, so `addn accounts remove <app>:<alias> --revoke` refuses when
  other apps of the same Google user would stop working, unless you add
  `--and-siblings` (they are then marked `needs-reauth`).
- **Built for agents.** Tool commands print JSON on stdout, errors are
  structured JSON on stderr, exit codes are stable, and files go in and out.

> **Status:** early development. Nothing is usable yet; the first usable
> release is `v0.1.0` ("Google for my agents"). See
> [`docs/cli-plan.md`](docs/cli-plan.md) for the plan.

## Development

```bash
go test ./...
go run ./cmd/addn version
```

The earlier TypeScript/Python SDK lives on the `archive/sdk-v0.1` branch.

## License

MIT
