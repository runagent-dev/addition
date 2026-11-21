Google Chat recipes for `addition call google-chat`: spaces, messages and sending, with the Workspace-only caveats.

# Google Chat

App `google-chat`, host `chat.googleapis.com`, paths `/v1/` and `/upload/v1/`. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/chat.spaces.readonly`, `https://www.googleapis.com/auth/chat.messages.readonly`, `https://www.googleapis.com/auth/chat.memberships.readonly`, `https://www.googleapis.com/auth/chat.messages.create`.

## Before it works

- Enable the **Google Chat API** in the Cloud project and configure a Chat app on its Configuration page. Until then calls fail with HTTP 403 and `SERVICE_DISABLED` in the body: it is setup, not permissions. Enable it, wait a minute, retry.
- Chat is mostly Workspace-only: consumer `@gmail.com` accounts usually cannot use the API. Connect a Workspace account.

```bash
addition call google-chat -a work GET /v1/spaces
addition call google-chat -a work GET /v1/spaces/<spaceId>/messages --query pageSize=50
addition call google-chat -a work GET /v1/spaces/<spaceId>/messages --query pageSize=50 --query pageToken=<nextPageToken>
addition call google-chat -a work POST /v1/spaces/<spaceId>/messages --data '{"text":"Build finished"}'
```

## Pitfalls

- Use `pageSize=50` or more for messages: a tiny page size can return an empty page that still has a `nextPageToken`. Keep following `nextPageToken` until it is absent; an empty page is not the end.
- `<spaceId>` is the part after `spaces/` in a space's `name`.
- An unknown or non-member space is 403 or 404.
