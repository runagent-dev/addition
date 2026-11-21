Google Drive recipes for `addition call drive`: list, search, metadata, create, download, pagination and the `fields` rule.

# Google Drive

App `google-drive` (command name `drive`), hosts `www.googleapis.com` and `drive.googleapis.com`, paths `/drive/`, `/upload/drive/`, `/download/drive/`. Enable the **Google Drive API** in the Cloud project first. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/drive`.

## Always pass `fields`

Drive v3 returns only `id`, `name`, `mimeType` and `kind` unless `fields` asks for more. Put it on every list and get. Nested fields use parentheses, not dots: `user(emailAddress)`, never `user.emailAddress`.

```bash
addition call drive -a work GET /drive/v3/about --query 'fields=user(emailAddress),storageQuota'
addition call drive -a work GET /drive/v3/files --query 'fields=files(id,name,mimeType),nextPageToken' --query pageSize=20
addition call drive -a work GET /drive/v3/files --query 'fields=files(id,name),nextPageToken' --query pageToken=<nextPageToken>
addition call drive -a work GET /drive/v3/files --query "q=name contains 'invoice' and trashed=false" --query 'fields=files(id,name,mimeType)'
addition call drive -a work GET /drive/v3/files/<fileId> --query 'fields=id,name,mimeType,size,modifiedTime,parents,webViewLink'
addition call drive -a work PATCH /drive/v3/files/<fileId> --data '{"trashed":true}' --query 'fields=id,trashed'
addition call drive -a work GET /drive/v3/files/<fileId> --query alt=media -o ./file.pdf
addition call drive -a work GET /drive/v3/files/<fileId>/export --query mimeType=application/pdf -o ./doc.pdf
```

Paginate with `nextPageToken` and `pageToken`. `alt=media` downloads stored files; Google Docs, Sheets and Slides need `/export` with a target `mimeType`. Binary bodies need `-o`.

## Pitfalls

- A missing `fields` is the usual reason a property is absent.
- DELETE removes a file permanently; PATCH `{"trashed":true}` uses the trash.
- Shared drives need `supportsAllDrives=true` (and `includeItemsFromAllDrives=true` on lists).
