# Addition

`addn` is a local-first CLI that gives your coding agents authenticated access
to your accounts — several Gmail inboxes, Drives and calendars, and later
GitHub, Slack and Discord — from any harness, on any machine.

```bash
addn connect google --as work --services gmail,drive,calendar
addn gmail search-messages -a work --query "has:attachment newer_than:7d"
addn gmail get-attachment  -a work --message <id> --attachment <id> -o ./invoice.pdf
addn drive upload          -a personal --file ./invoice.pdf --folder "Taxes/2026"
addn call -a work GET /calendar/v3/calendars/primary/events
```

- **Local only.** Credentials live in one encrypted SQLite file on your machine.
  No server, no account, no telemetry.
- **One binary.** Static Go build for Linux, macOS and Windows.
- **Built for agents.** JSON on stdout, structured errors on stderr, stable exit
  codes, files in and out.

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
