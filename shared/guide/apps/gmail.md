Gmail recipes for `addition call gmail`: profile, search, read, send, attachments, pagination and pitfalls.

# Gmail

App `gmail`, host `gmail.googleapis.com`, paths `/gmail/` and `/upload/gmail/`. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/gmail.readonly`, `https://www.googleapis.com/auth/gmail.send`, `https://www.googleapis.com/auth/gmail.modify`.

```bash
addition call gmail -a work GET /gmail/v1/users/me/profile
addition call gmail -a work GET /gmail/v1/users/me/messages --query 'q=has:attachment newer_than:7d' --query maxResults=10
addition call gmail -a work GET /gmail/v1/users/me/messages --query 'q=has:attachment newer_than:7d' --query maxResults=10 --query pageToken="$PAGE_TOKEN"
addition call gmail -a work GET "/gmail/v1/users/me/messages/$MESSAGE_ID" --query format=metadata --query metadataHeaders=Subject --query metadataHeaders=From
addition call gmail -a work GET "/gmail/v1/users/me/messages/$MESSAGE_ID/attachments/$ATTACHMENT_ID"
addition call gmail -a work POST /gmail/v1/users/me/messages/send --data "{\"raw\":\"$RAW_MESSAGE\"}"
```

`q` uses Gmail search syntax. A list returns only `id` and `threadId`; fetch each message for content. More results exist while `nextPageToken` is present: repeat the same search (same `q`, same `maxResults`) with `pageToken` set to it. Variables such as `$MESSAGE_ID`, and `$RAW_MESSAGE` (the base64url RFC 2822 message), stand for values you fill in; keep them double-quoted.

## Pitfalls

- **Bodies are base64url.** Text is in `payload.parts[].body.data` (or `payload.body.data` without parts), with `-` and `_` and no padding; decode before reading. Plain text and HTML are separate parts.
- **An invalid message id returns 400** (`Invalid id value`, exit 4); a well-formed id that does not exist returns 404.
- An attachment is JSON with base64url `data`; its id is `payload.parts[].body.attachmentId`.
- Sending or modifying needs `gmail.send` or `gmail.modify`; if `accounts show` lists `missing_scopes`, reconnect.
