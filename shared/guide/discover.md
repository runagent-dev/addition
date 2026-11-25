Find the API operation you need without knowing the API: search, describe, then call; how to read effect and scopes.

# Discovering what you can do

You do not need to know an app's API in advance. Two commands answer "what can I do?" and both work offline, with no account and no vault:

```bash
addition search send an email                  # ranked operations: op, summary, effect, method, path
addition describe gmail.users.messages.send    # parameters, body fields, scopes and the call to run
addition call gmail -a work POST "/gmail/v1/users/me/messages/send" --data "$JSON"
```

1. **Search** with the words for what you want to do. Each result has an `op` id, a one-line `summary`, an `effect`, the `method` and `path`, the `required` parameters (those without a default) and the exact `describe` command. The best match is first.
2. **Describe** one `op`. You get every parameter (`in` is `path` or `query`), the request `body` schema with its top-level fields, `scopes_any_of` and a ready `call` template. Replace `<alias>` with the connection's alias, fill each `$PLACEHOLDER` and `<json>`, and run it.
3. **Call** it with `addition call` (`addition guide call`). Everything else, tokens and refresh included, is handled for you.

## Reading a result

- **`effect`** is `read` (changes nothing), `write` (creates or changes something) or `destructive` (deletes or empties). It is derived from the HTTP method and the operation's name, and anything unclear counts as `write`. Prefer `--effect read` while exploring. Do not run a `write` or `destructive` operation unless the user asked for that change.
- **`scopes_any_of`**: the account needs any one of them. Compare with `addition accounts show`: `scopes` is what the account holds, and `missing_scopes` lists the app's default scopes it lacks (`addition guide accounts`). If the call returns 403 for scopes, reconnect the app and tick every box.
- **`app`** is the app to give `addition call`, and the one whose account you need connected.
- `total` counts the relevant matches; `truncated: true` means more exist than were returned, and `narrow` says how to narrow them.

## Narrowing and listing

```bash
addition search                                # no query: every app and how many operations it has, by effect
addition search events --app google-calendar   # only one app
addition search list --effect read --limit 20  # only safe operations, more results (1-25, default 8)
```

No matches is not an error: the output has empty `results`, the list of apps and a `next` command. Rephrase with the words the app uses ("message" for mail, "event" for a calendar entry). An unknown op id given to `describe` exits 2 (`unknown-operation`) and names the closest ids.

The operation list is what each app's own API description publishes (hundreds of operations), imported into this binary. It tells you what exists; the app's recipe (`addition guide gmail`) says which operations are worth using and the pitfalls.
