Google Drive recipes for `addition call drive`: list, search, metadata, create, download, pagination and the `fields` rule.

# Google Drive

App `google-drive` (command name `drive`), hosts `www.googleapis.com` and `drive.googleapis.com`, paths `/drive/`, `/upload/drive/`, `/download/drive/`. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/drive`.

## Always pass `fields`

`about.get` requires `fields`. `files.list` and `files.get` return only `id`, `name`, `mimeType` and `kind` by default, so an explicit mask is better. Put it on every call, with `nextPageToken` when you paginate. Nested fields use parentheses: `user(emailAddress)`, not dots.

```bash
addition call drive -a work GET /drive/v3/about --query 'fields=user(emailAddress),storageQuota'
addition call drive -a work GET /drive/v3/files --query 'fields=files(id,name,mimeType),nextPageToken' --query pageSize=20
addition call drive -a work GET /drive/v3/files --query pageSize=20 --query 'fields=files(id,name,mimeType),nextPageToken' --query pageToken="$PAGE_TOKEN"
addition call drive -a work GET /drive/v3/files --query "q=name contains 'invoice' and trashed=false" --query 'fields=nextPageToken,files(id,name,mimeType)'
addition call drive -a work GET "/drive/v3/files/$FILE_ID" --query 'fields=id,name,mimeType,size,modifiedTime,parents,webViewLink'
addition call drive -a work GET "/drive/v3/files/$FILE_ID" --query alt=media -o ./file.pdf
addition call drive -a work GET "/drive/v3/files/$FILE_ID/export" --query mimeType=application/pdf -o ./doc.pdf
```

Paginate with `nextPageToken` and `pageToken`: later pages repeat the same `q` and `fields` mask with `pageToken` set. `$FILE_ID` stands for a value you fill in; keep it double-quoted. `alt=media` downloads stored files; Google Docs, Sheets and Slides need `/export` with a target `mimeType`. Binary bodies need `-o`.

## Pitfalls

- A missing `fields` is the usual reason a property is absent.
- DELETE removes a file permanently; to use the trash, PATCH `{"trashed":true}`.
- Shared drives need `supportsAllDrives=true` (and `includeItemsFromAllDrives=true` on lists).
