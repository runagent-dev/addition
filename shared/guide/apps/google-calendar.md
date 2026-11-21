Google Calendar recipes for `addition call calendar`: calendars, events in a window, creating events and RFC3339 times.

# Google Calendar

App `google-calendar` (command name `calendar`), host `www.googleapis.com`, paths `/calendar/`. Enable the **Google Calendar API** in the Cloud project first. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/calendar`, `https://www.googleapis.com/auth/calendar.events`.

```bash
addition call calendar -a work GET /calendar/v3/users/me/calendarList
addition call calendar -a work GET /calendar/v3/calendars/primary/events --query timeMin=2026-01-05T00:00:00Z --query timeMax=2026-01-12T00:00:00Z --query singleEvents=true --query orderBy=startTime
addition call calendar -a work POST /calendar/v3/calendars/primary/events --data '{"summary":"Review","start":{"dateTime":"2026-01-06T10:00:00+01:00"},"end":{"dateTime":"2026-01-06T10:30:00+01:00"}}'
addition call calendar -a work PATCH /calendar/v3/calendars/primary/events/<eventId> --data '{"summary":"Review (moved)"}'
addition call calendar -a work DELETE /calendar/v3/calendars/primary/events/<eventId>
```

## Pitfalls

- `timeMin` and `timeMax` must be RFC3339 with a zone (`2026-01-05T00:00:00Z` or `…+01:00`); a bare date is a 400.
- `orderBy=startTime` requires `singleEvents=true`; without it recurring events come back as one rule, not occurrences.
- Paginate with `nextPageToken` and `pageToken`.
- `primary` is the account's main calendar; others use the `id` from `calendarList`.
- All-day events use `{"date":"2026-01-06"}` instead of `dateTime`. A successful DELETE has an empty body.
