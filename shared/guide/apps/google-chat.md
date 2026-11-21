Google Chat recipes for `addition call google-chat`: spaces, messages and sending, with the Workspace-only caveats.

# Google Chat

App `google-chat`, host `chat.googleapis.com`, paths `/v1/` and `/upload/v1/`. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/chat.spaces.readonly`, `https://www.googleapis.com/auth/chat.messages.readonly`, `https://www.googleapis.com/auth/chat.memberships.readonly`, `https://www.googleapis.com/auth/chat.messages.create`.

## Before it works

- `SERVICE_DISABLED` (HTTP 403) means the **Google Chat API** is not enabled in the user's Cloud project: enable it, wait a minute, retry.
- Some calls also need a Chat app configuration in the Cloud project (the Chat API's Configuration page; see Google's "Configure the Chat API" guide). With the API enabled, a missing app configuration is a different failure from `SERVICE_DISABLED`: read the error body.
- Chat is mostly Workspace-only: consumer `@gmail.com` accounts usually cannot use the API.

```bash
addition call google-chat -a work GET /v1/spaces
addition call google-chat -a work GET "/v1/spaces/$SPACE_ID/messages" --query pageSize=50
addition call google-chat -a work GET "/v1/spaces/$SPACE_ID/messages" --query pageSize=50 --query pageToken="$PAGE_TOKEN"
addition call google-chat -a work POST "/v1/spaces/$SPACE_ID/messages" --data '{"text":"Build finished"}'
```

## Pitfalls

- `pageSize=50` is a good page size. At any page size, a returned `nextPageToken` decides whether another page exists: keep following it until it is absent, and do not stop at an empty page.
- `$SPACE_ID` and `$PAGE_TOKEN` stand for values you fill in; keep them double-quoted.
- A space id is the part after `spaces/` in its `name`.
