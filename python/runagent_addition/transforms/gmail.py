from __future__ import annotations

import base64
from typing import Any, Dict


def _rfc2822(params: Dict[str, Any]) -> Dict[str, Any]:
    """Build a base64url-encoded RFC 2822 MIME message for Gmail's send API.

    NOTE: headers and body are separated by an explicit CRLF blank line
    (\\r\\n\\r\\n) per RFC 2822 §2.1. Do NOT use filter(bool, [..., '', body]) —
    the empty-string separator is falsy and would be stripped, producing a
    malformed MIME message Gmail rejects.
    """
    to: str = params["to"]
    subject: str = params["subject"]
    body: str = params["body"]
    cc: str | None = params.get("cc")
    bcc: str | None = params.get("bcc")

    headers: list[str] = [f"To: {to}"]
    if cc:
        headers.append(f"Cc: {cc}")
    if bcc:
        headers.append(f"Bcc: {bcc}")
    headers.append(f"Subject: {subject}")
    headers.append("MIME-Version: 1.0")
    headers.append('Content-Type: text/plain; charset="UTF-8"')

    message = "\r\n".join(headers) + "\r\n\r\n" + body
    raw = base64.urlsafe_b64encode(message.encode("utf-8")).rstrip(b"=").decode("ascii")
    return {"body": {"raw": raw}}


gmail_transforms = {
    "gmail:rfc2822": _rfc2822,
}
