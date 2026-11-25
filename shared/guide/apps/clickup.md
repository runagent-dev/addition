ClickUp recipes for `addition call clickup`: connect with a personal token, find the Workspace id, tasks, comments, pagination and rate limits.

# ClickUp

App `clickup` (no command name; use `addition call`), host `api.clickup.com`, paths `/api/`. It connects with a personal token and no OAuth client. One token is one ClickUp user; connect each user under its own alias.

## Connect

The human creates the token: Settings → Apps → API Token → Generate, at `https://app.clickup.com/settings/apps`. It starts with `pk_` and never expires; Regenerate replaces it. Give it on standard input, never as a flag:

```bash
read -rs TOKEN
printf '%s\n' "$TOKEN" | addition connect clickup --as work --token-stdin
addition connect clickup --as client --token-env CLICKUP_TOKEN
```

The account is named by the user's email. Another user's token on an existing alias fails with `identity-mismatch`. Addition sends the token alone in `Authorization`, without `Bearer`, as ClickUp expects.

## Start with the Workspace id

A ClickUp Workspace is a *team* in the API. Almost every route needs its id as `team_id`, so list it first:

```bash
addition call clickup -a work GET /api/v2/team
```

The reply is `{"teams":[{"id":"…","name":"…"}]}`. A token sees every Workspace its user belongs to.

## Spaces, folders, lists and tasks

```bash
addition call clickup -a work GET "/api/v2/team/$TEAM_ID/space" --query archived=false
addition call clickup -a work GET "/api/v2/space/$SPACE_ID/folder" --query archived=false
addition call clickup -a work GET "/api/v2/folder/$FOLDER_ID/list" --query archived=false
addition call clickup -a work GET "/api/v2/list/$LIST_ID/task" --query page=0 --query include_closed=true
addition call clickup -a work GET "/api/v2/team/$TEAM_ID/task" --query 'assignees[]=123' --query page=0
addition call clickup -a work GET "/api/v2/task/$TASK_ID"
addition call clickup -a work GET "/api/v2/task/$TASK_ID/comment"
```

`$TEAM_ID`, `$SPACE_ID` and the like stand for values you fill in; keep them double-quoted. Lists without a folder are under the space: `GET /api/v2/space/$SPACE_ID/list`.

Create and change things with a JSON body (`addition guide call` shows how a body is passed):

```bash
addition call clickup -a work POST "/api/v2/list/$LIST_ID/task" --data '{"name":"Write the report","status":"to do"}'
addition call clickup -a work PUT "/api/v2/task/$TASK_ID" --data '{"status":"in progress"}'
addition call clickup -a work POST "/api/v2/task/$TASK_ID/comment" --data '{"comment_text":"On it."}'
```

Find the exact route and fields with `addition search` and `addition describe` once ClickUp's operations are imported; until then, ClickUp's own reference is `https://developer.clickup.com/reference`.

## Pagination

Task lists return up to 100 tasks a page. Repeat the same query with `--query page=1`, `page=2`, and so on, until a page comes back with fewer tasks (the reply also has `last_page` on some routes).

## Rate limits

The limit is per token: 100 requests a minute on Free, Unlimited and Business, 1,000 on Business Plus, 10,000 on Enterprise. When it is exceeded the API answers `429`. addition retries a `429` with backoff; ClickUp sends the reset time as a Unix timestamp in `X-RateLimit-Reset`, not `Retry-After`, so the wait is a growing backoff rather than the exact reset. Keep loops sequential and wait on a long `429`.

## Pitfalls

- A `401` or `OAUTH_019` ("Token invalid") means the token was regenerated or revoked: the account becomes `needs-reauth`. Create a new token and run the same `connect` again.
- Regenerating a token in ClickUp invalidates the old one at once, for every machine that holds it.
- A task id in a URL is the task's id, not its custom id; custom ids need `?custom_task_ids=true&team_id=$TEAM_ID`.
- DELETE is permanent.
