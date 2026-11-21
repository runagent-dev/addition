List, inspect, check, default and remove connected accounts, and read their status and scope fields.

# Managing accounts

An account is `<app>:<alias>`, for example `gmail:work`. These print JSON and read only the vault, except `--live` and `--revoke`.

```bash
addition accounts list                       # every account
addition accounts show gmail:work            # one account
addition accounts check                      # token and scope status, vault only
addition accounts check gmail:work --live    # also refresh and ask Google whether the token works
addition accounts default gmail:work         # used when a call names no -a
addition accounts remove gmail:work          # delete it and its tokens locally; nothing is sent
```

## Fields

`list` and `show` rows: `provider`, `alias`, `identity` (the email), `scopes` (granted), `scopes_ready`, `missing_scopes`, `status`, `expires_at`, `has_refresh_token`, `default`.

| `status` | Meaning | Do this |
|---|---|---|
| `active` | Usable | Nothing |
| `needs-reauth` | Google refused the refresh token, or it was revoked | `addition connect <app> --as <alias>` |
| `legacy` | Connected before every app became its own connection (`google:<alias>`) | `addition accounts remove google:<alias>`, then connect each app you use |

- `scopes_ready` is true when the account holds every default scope of its app. When false, `missing_scopes` lists the scopes it was not granted; reconnect with `addition connect <app> --as <alias>` and keep every box ticked.
- `accounts check` rows add `connected`, `access_token` (`valid`, `expired` or `missing`, never the token) and, with `--live`, `live` and `live_reason` (`ok`, `needs-reauth`, `token-rejected`, `network`, …).

## Removing

```bash
addition accounts remove gmail:work                              # local only
addition accounts remove gmail:work --revoke                     # also ask Google to revoke
addition accounts remove gmail:work --revoke --and-siblings      # other apps stop working too
```

Google revokes a whole grant at once: every app the same user connected through the same OAuth client. So `--revoke` on a Google app refuses with `revoke-affects-other-apps` (exit 2), naming the others. `--and-siblings` revokes anyway and marks them `needs-reauth` (listed in `also_invalidated`). To drop one app alone, omit `--revoke`.
