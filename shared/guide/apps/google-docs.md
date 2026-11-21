Google Docs recipes for `addition call google-docs`: read a document, create one and edit it with batchUpdate.

# Google Docs

App `google-docs`, host `docs.googleapis.com`, paths `/v1/documents`. Scopes requested: `openid`, `https://www.googleapis.com/auth/userinfo.email`, `https://www.googleapis.com/auth/documents`. Find document ids with the Drive recipe (`addition guide google-drive`): a Doc has mimeType `application/vnd.google-apps.document`.

```bash
addition call google-docs -a work GET "/v1/documents/$DOCUMENT_ID"
addition call google-docs -a work POST /v1/documents --data '{"title":"Meeting notes"}'
addition call google-docs -a work POST "/v1/documents/$DOCUMENT_ID:batchUpdate" --data '{"requests":[{"insertText":{"location":{"index":1},"text":"Hello\n"}}]}'
```

## Pitfalls

- Text sits in `body.content[].paragraph.elements[].textRun.content`.
- Every edit goes through `:batchUpdate` with a list of `requests`. Indexes count UTF-16 units; the body starts at 1 and ends at the last `endIndex`, so append at `endIndex - 1`.
- `$DOCUMENT_ID` stands for the document id; keep it double-quoted.
- The colon in `:batchUpdate` is part of the path; do not escape it.
- This scope covers Docs only: renaming, moving or sharing a document is a Drive call.
