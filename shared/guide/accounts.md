List, inspect, check, default and remove connected accounts, and read their status and scope fields.

# Managing accounts

An account is `<app>:<alias>`, e.g. `gmail:work`. These print JSON; only `--live` and `--revoke` leave the machine.

```bash
addition accounts list
addition accounts show gmail:work
addition accounts check                      # vault only
addition accounts check gmail:work --live    # also refreshes and asks Google
addition accounts default gmail:work         # used when a call names no -a
addition accounts remove gmail:work          # local only
```

## Fields

`list` and `show` rows: `provider`, `alias`, `identity` (email), `scopes`, `scopes_ready`, `missing_scopes`, `status`, `expires_at`, `has_refresh_token`, `default`, `created_at`, `updated_at`.

| `status` | Meaning | Do this |
|---|---|---|
| `active` | The stored state, not proof: it can still have `live: false` or missing scopes | `accounts check --live` |
| `needs-reauth` | The refresh token was refused or revoked | `addition connect <app> --as <alias>` |
| `legacy` | Predates per-app connections (`google:<alias>`) | Remove it, then connect each app you use |

- `scopes_ready` is true when the account holds every default scope of its app; if not, `missing_scopes` lists them: reconnect and tick every box.
- `check` rows are `provider`, `alias`, `connected`, `status`, `access_token`, `expires_at`, `has_refresh_token`, `scopes`, `scopes_ready`, `missing_scopes` and, with `--live`, `live`, `live_reason`; they omit `identity` and `default`.
- `access_token` is `valid`, `expiring` (within 5 minutes of expiry), `expired`, `no-expiry` (the provider gave no expiry) or `missing`; the token itself is never printed.
- `live` is `true` when refresh and identity lookup worked; else `live_reason` says why (`needs-reauth`, `token-rejected`, `network`, …).
- `list` and a bare `check` report a legacy row (status `legacy`); `show` or `check` naming it (`google:<alias>`) exits 3 with `legacy-account`.

## Removing

```bash
addition accounts remove gmail:work --revoke                 # also revoke at Google
addition accounts remove gmail:work --revoke --and-siblings  # other apps stop working too
```

Two separate things:

- **The local guard.** addition knows the same user's other apps in this vault share one grant: `--revoke` refuses with `revoke-affects-other-apps` (exit 2), naming them; `--and-siblings` revokes anyway and marks them `needs-reauth` (`also_invalidated`).
- **Google's side.** Google revokes a whole grant at once, which can reach beyond this vault: the same user's grant can be revoked across the OAuth clients of the same Cloud project. To drop one app alone, omit `--revoke`.
