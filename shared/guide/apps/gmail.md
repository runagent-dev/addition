Gmail recipes for `addition call gmail`: profile, search, read, send, attachments, pagination and pitfalls.

# Gmail

App `gmail`, host `gmail.googleapis.com`, paths `/gmail/` and `/upload/gmail/`. Enable the **Gmail API** in the Cloud project first. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/gmail.readonly`, `https://www.googleapis.com/auth/gmail.send`, `https://www.googleapis.com/auth/gmail.modify`.

```bash
addition call gmail -a work GET /gmail/v1/users/me/profile
addition call gmail -a work GET /gmail/v1/users/me/messages --query 'q=has:attachment newer_than:7d' --query maxResults=10
addition call gmail -a work GET /gmail/v1/users/me/messages --query 'q=is:unread' --query pageToken=<nextPageToken>
addition call gmail -a work GET /gmail/v1/users/me/messages/<id> --query format=metadata --query metadataHeaders=Subject --query metadataHeaders=From
addition call gmail -a work GET /gmail/v1/users/me/messages/<id> --query format=full
addition call gmail -a work GET /gmail/v1/users/me/messages/<id>/attachments/<attachmentId>
addition call gmail -a work POST /gmail/v1/users/me/messages/send --data '{"raw":"<base64url RFC 2822 message>"}'
```

`q` uses Gmail search syntax. A list returns only `id` and `threadId`; fetch each message for content. More results exist while `nextPageToken` is present.

## Pitfalls

- **Bodies are base64url.** Text is in `payload.parts[].body.data` (or `payload.body.data` without parts), with `-` and `_` and no padding; decode before reading. Plain text and HTML are separate parts.
- **An invalid message id returns 400** (`Invalid id value`, exit 4); a well-formed id that does not exist returns 404.
- An attachment response is JSON with base64url `data`; the `attachmentId` is in `payload.parts[].body.attachmentId`.
- Sending or modifying needs `gmail.send` or `gmail.modify`; if `accounts show` lists `missing_scopes`, reconnect.
