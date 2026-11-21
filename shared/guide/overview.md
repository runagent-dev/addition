What addition is, how to think about it, and the first run: install, init, client add, connect, call.

# Addition: overview

`addition` gives you, a coding agent, authenticated access to the user's accounts (Gmail, Drive, Calendar, Chat, Docs) through one local CLI. Tokens stay in a local vault and are never printed; no server of ours is involved.

## Mental model

- **Connection = app + alias.** Each app (`gmail`, `google-drive`, `google-calendar`, `google-chat`, `google-docs`) is connected separately under an alias (`work`): `gmail:work` and `google-drive:work` are two connections with their own scopes.
- **The vault** is one SQLite file under `$ADDITION_HOME`. Secrets in it (the OAuth client secret, access and refresh tokens) are encrypted at the field level with a master key kept outside the file.
- **Output contract.** Data commands print JSON on stdout. Exceptions: `addition guide` and `--help` print text, and `addition call` prints the upstream body exactly as received (JSON, raw text or nothing; see `addition guide call`). A failure prints `{"error":{"code","message"}}` on stderr and exits non-zero (`addition guide errors`). Commands never prompt unless stdin is a terminal.

## First run

```bash
# 1. install (once); if the installer printed "on_path": false, use the printed full path or add its directory to PATH
curl -fsSL https://raw.githubusercontent.com/runagent-dev/addition/main/install.sh | sh
# 2. create the vault; on a host without a keychain, plain `addition init` fails with keychain-unavailable (exit 2)
addition init --key-file
# 2, alternative: take the master key from the environment (64 hex characters) for this and every later command
#   export ADDITION_MASTER_KEY="$(openssl rand -hex 32)"
#   addition init
# 3. store the user's own Google OAuth client (a Desktop-app client_secret.json)
addition client add google --file client_secret.json
# 4. connect headlessly: print the link, the user opens authorize_url and approves, then paste the address-bar URL
addition connect gmail --as work --no-browser
read -r URL
addition connect --complete "$URL"
# 5. call its API
addition call gmail -a work GET /gmail/v1/users/me/profile
```

Step 2 is the headless form. Where a Secret Service or keychain exists, plain `addition init` keeps the key there; on a desktop `addition connect gmail --as work` opens the browser itself (`addition guide connect`). `--key-file` keeps the key in `$ADDITION_HOME/master.key` (mode 0600) and is not supported on Windows, which uses the Credential Manager. The `read -r URL` step takes the callback URL from stdin, never from a command line. When something fails, `addition doctor` reports the vault, key source and accounts once it is in your build; `addition guide troubleshooting` covers the rest.

No Google Cloud project yet? See `addition guide troubleshooting`.

Next: `addition guide call`, then the app's recipe (`addition guide gmail`). Without `-a`, `call` uses the app's only account or its default.
