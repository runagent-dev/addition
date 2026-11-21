The contract of `addition call`: arguments, output, exit codes and what it refuses.

# addition call

```text
addition call <app> [-a <alias>] <METHOD> <path> [--query k=v]... [--data @file|-|'<json>'] [-H 'Name: value']... [-o file] [--force]
```

- `<app>`: a provider id or command name from `addition guide apps` (`gmail`, `drive` or `google-drive`, `calendar`, `google-chat`, `google-docs`).
- `-a, --account <alias>`: optional when the app has one account or a default (`addition accounts default <app>:<alias>`).
- `<METHOD>`: `GET HEAD POST PUT PATCH DELETE OPTIONS`.
- `<path>`: absolute from the API host (`/gmail/v1/users/me/profile`) or an https URL on the app's own hosts and path prefixes. Never put `?` in it.
- `--query k=v`: repeatable; values are URL-encoded for you.
- `--data`: body as `@file`, `-` (stdin) or the JSON itself; Content-Type defaults to `application/json`, override with `-H`.
- `-H 'Name: value'`: repeatable; `Authorization`, `Host`, `Cookie` and hop-by-hop headers are addition's (`forbidden-header`).
- `-o, --output <file>`: write a 2xx body to a file (mode 0600). An existing file needs `--force`.

```bash
addition call google-drive -a work GET /drive/v3/files --query 'fields=files(id,name)' --query pageSize=5
addition call google-calendar -a work POST /calendar/v3/calendars/primary/events --data @event.json
```

## Output

- **2xx text or JSON:** the body goes to stdout verbatim, exit 0.
- **2xx binary:** needs `-o`; without it you get `binary-response` (exit 2). With `-o` stdout is `{"status","content_type","bytes","path"}`.
- **Non-2xx:** the upstream body goes to stdout (Google puts the reason in it) and stderr carries `{"error":{"code":"upstream-error","message":"HTTP 404"}}`. Read stdout for the cause.
- **No redirects** are followed (a 3xx is exit 4). A response that contains the access token, or has a content coding, is withheld (`credential-echo`, `unsupported-encoding`).
- addition renews an expiring token, retries once on a 401 and waits out a 429. Do not loop on exit 3.

## Exit codes

| Exit | Meaning |
|---|---|
| 0 | ok |
| 1 | internal: addition itself failed, was interrupted, or the vault is busy |
| 2 | usage: bad arguments, unknown app, path not allowed |
| 3 | not connected or needs reconnecting: run `addition connect` |
| 4 | upstream 4xx (or a 3xx) |
| 5 | upstream 5xx, or the network |
| 6 | policy denied (reserved; nothing returns it yet) |

Error codes and fixes: `addition guide errors`. Paginate with the API's own `pageToken`; addition does not do it for you. Request bodies are capped at 64 MiB.
