Connect an app's account: OAuth on a headless machine (hand the consent link to the human), or an API token the human creates.

# Connecting an account

Prerequisite: `addition init`. OAuth apps (the Google ones) also need `addition client add google --file client_secret.json`; token apps need no OAuth client. Each app is connected separately. `addition providers` lists each app's `auth_methods` (`oauth2`, `api_key`) and its `auth_type`, the method `connect` uses when you name none.

```bash
addition connect gmail --as work
```

This opens the consent screen in a browser and waits 5 minutes for the redirect. With no browser here, use the two-step flow.

## Headless, remote or agent flow

```bash
addition connect gmail --as work --no-browser
```

stdout is `{"authorize_url":"https://accounts.google.com/…","expires_at":"…","next":"addition connect --complete '<redirect-url>'"}`.

1. Give `authorize_url` to the human to open on any device and approve.
2. The browser lands on `http://127.0.0.1:<port>/callback?code=…&state=…`; the page fails to load, as expected. Ask for the full address-bar URL.
3. Have the human paste it into a private input, not a command line, and pass it as one argument:

```bash
read -r URL
addition connect --complete "$URL"
```

`read -r URL` takes the URL from stdin (a private prompt or secret input), so it is never typed into a command. stdout is `{"provider","alias","identity","scopes","created"}`.

A link works once and expires after 10 minutes; then run `--no-browser` again. The callback URL carries a one-use authorization code: keep it out of logs, shell history and transcripts.

In browser mode, interrupting the command after an `open-browser-failed` notice exits 2 with `usage` ("context canceled"), not 1; run it again or use `--no-browser`.

## With a token (API key or personal access token)

Some apps, such as `github`, connect with a token the human creates in the provider's own settings (the app's `api_key.create_url`, shown in the prompt and in `token-required`). No OAuth client is needed. The token is never accepted as a flag value, because `ps` and shell history would show it. Give it on standard input or through an environment variable:

```bash
read -rs TOKEN
printf '%s\n' "$TOKEN" | addition connect github --as me --token-stdin
addition connect github --as me --token-env GITHUB_TOKEN
```

- `read -rs TOKEN` takes the token from a private prompt (the human types or pastes it; nothing is echoed). `--token-stdin` reads one line, trimmed; `--token-env NAME` reads the variable called `NAME` (the app suggests one in `api_key.env_var`).
- At a terminal, with neither flag, `connect` asks for the token without echo. It never prompts when stdin is not a terminal: without a flag it exits 2 with `token-required`.
- Before storing anything, addition checks the token with one `GET` of the app's identity endpoint, through the same engine as `addition call` (allowed hosts and paths, no redirects, the echo guard). A token the app refuses (HTTP 401 or 403) is not stored: exit 3 with `invalid-token`, naming where to create a new one.
- `--token-stdin` and `--token-env` select the `api_key` method; `--method api_key` or `--method oauth2` picks one when an app offers both (`--no-browser` implies `oauth2`). An app without that method fails with `method-not-supported` (exit 2). A token with the wrong prefix for the app fails with `token-prefix-mismatch` before anything is sent.
- stdout is `{"provider","alias","identity","scopes","created","method"}` with `"method":"api_key"` and `"scopes":[]`: a token has no scopes to grant.
- A token does not refresh or expire by itself. When the provider rejects it later, the call exits 3 with `needs-reauth` and the account is marked `needs-reauth`: create a new token and run the same `connect` again; the alias keeps its name and gets a new credential.

## When it fails

Common codes: `no-oauth-client` (run `client add`), `bad-redirect` (ask for the whole address-bar URL), `unknown-state`, `state-mismatch`, `authorization-expired` (link used or too old: run `--no-browser` again), `access-denied` (the human cancelled), `scopes-not-granted` (saved, but permissions were unticked: connect again, tick all), `identity-mismatch` (that alias belongs to another account), and, for tokens, `invalid-token` (the app refused it: create a new one), `token-required` (pass `--token-stdin` or `--token-env`), `empty-token`, `bad-token`, `token-env-unset`, `token-prefix-mismatch`, `method-not-supported`. All codes: `addition guide errors`. Verify with `addition accounts check --live`.
