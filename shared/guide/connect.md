Connect an app's account on a headless machine: hand the consent link to the human and finish with the pasted URL.

# Connecting an account

Prerequisite: `addition init` and `addition client add google --file client_secret.json`. Each app is connected separately.

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

## When it fails

Common codes: `no-oauth-client` (run `client add`), `bad-redirect` (ask for the whole address-bar URL), `unknown-state`, `state-mismatch`, `authorization-expired` (link used or too old: run `--no-browser` again), `access-denied` (the human cancelled), `scopes-not-granted` (saved, but permissions were unticked: connect again, tick all), `identity-mismatch` (that alias belongs to another account). All codes: `addition guide errors`. Verify with `addition accounts check --live`.
