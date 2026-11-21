What addition is, how to think about it, and the six steps of a first run.

# Addition: overview

`addition` gives you, a coding agent, authenticated access to the user's accounts (Gmail, Drive, Calendar, Chat, Docs) through one local CLI. Tokens stay in an encrypted vault on this machine and are never printed. No server of ours is involved.

## Mental model

- **Connection = app + alias.** Each app (`gmail`, `google-drive`, `google-calendar`, `google-chat`, `google-docs`) is connected separately under an alias the user picks (`work`). `gmail:work` and `google-drive:work` are two connections with their own scopes.
- **The vault** is one encrypted SQLite file under `$ADDITION_HOME` with the OAuth client, accounts and tokens.
- **`addition call` is the universal escape hatch**: one authenticated request to any endpoint of a connected app's API.
- **Output contract.** Commands print JSON on stdout. A failure prints `{"error":{"code","message"}}` on stderr and exits non-zero (`addition guide errors`). Only `addition guide` prints Markdown. Commands never prompt unless stdin is a terminal.

## First run

```bash
# 1. install (once)
curl -fsSL https://raw.githubusercontent.com/runagent-dev/addition/main/install.sh | sh
# 2. create the vault and its master key
addition init
# 3. run `addition doctor`, the first diagnostic command, now and whenever something fails
# 4. store the user's own Google OAuth client (a Desktop-app client_secret.json)
addition client add google --file client_secret.json
# 5. connect an app under an alias (headless: `addition guide connect`)
addition connect gmail --as work
# 6. call its API
addition call gmail -a work GET /gmail/v1/users/me/profile
```

No Google Cloud project yet? `addition guide troubleshooting`.

## Next

`addition guide call` (the contract), then the app's recipe (`addition guide gmail`); `errors` when a command fails; `accounts` to manage connections; `apps` for the list. Without `-a`, `call` uses the app's only account or its default.
