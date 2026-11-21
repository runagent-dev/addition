Connect an app's account on a headless machine: hand the consent link to the human and finish with the pasted URL.

# Connecting an account

Prerequisite: `addition init` and `addition client add google --file client_secret.json`. Each app is connected separately; an alias can repeat across apps.

```bash
addition connect gmail --as work
```

This opens the consent screen in a browser and waits 5 minutes for the redirect to `127.0.0.1`. Without a browser on this machine, use the two-step flow.

## Headless, remote or agent flow

```bash
addition connect gmail --as work --no-browser
```

stdout is `{"authorize_url":"https://accounts.google.com/…","expires_at":"…","next":"addition connect --complete '<redirect-url>'"}`.

1. Give `authorize_url` to the human to open on any device and approve.
2. The browser then lands on `http://127.0.0.1:<port>/callback?code=…&state=…`. That page fails to load, as expected; the address-bar URL is what you need.
3. Ask the human to paste that whole URL, then run it through single quotes (it contains `&`):

```bash
addition connect --complete 'http://127.0.0.1:<port>/callback?code=…&state=…'
```

stdout is `{"provider","alias","identity","scopes","created"}`. Suggested ask: "Open this link, pick the Google account, approve, then copy the full address of the page that fails to load (it starts with http://127.0.0.1) and paste it here."

Rules: a link works once and expires after 10 minutes; after that run `--no-browser` again. The pasted URL holds a one-time code: do not log it. Reconnecting an alias asks for the scopes it already has plus the app's own.

## When it fails

| `error.code` | Do this |
|---|---|
| `no-oauth-client` | `addition client add google --file client_secret.json` |
| `bad-redirect` | Ask for the whole address-bar URL, with `?code=…&state=…` |
| `unknown-state`, `state-mismatch`, `authorization-expired` | Link used, unknown or too old: run `--no-browser` again |
| `authorization-in-progress` | Another `--complete` has it; wait, or restart |
| `access-denied` | The human cancelled; ask whether to retry |
| `scopes-not-granted` | Saved, but permissions were unticked: connect again, tick all |
| `identity-mismatch` | The alias belongs to another account; use another alias |

All codes: `addition guide errors`. Verify afterwards with `addition accounts check --live`.
