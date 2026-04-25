from __future__ import annotations

import copy
from typing import Any, Dict, Optional


class DefaultAdapter:
    """Standard OAuth2 adapter — no provider-specific quirks.

    Scope (CLAUDE.md §6 invariant #10): request-shaping and error detection
    only. Token extraction is data-driven via provider YAML
    `token_response_mapping` and executed by the OAuth engine. This class
    MUST NOT implement `parse_token_response`.
    """

    def before_token_exchange(self, req: Dict[str, Any]) -> Dict[str, Any]:
        """Return req unchanged — default is a no-op."""
        return req

    def is_error_response(self, status: int, body: Any) -> bool:
        return status >= 400

    def extract_error_detail(self, body: Any) -> Dict[str, Any]:
        if body and isinstance(body, dict):
            if isinstance(body.get("error"), str):
                detail: Dict[str, Any] = {"code": body["error"]}
                if isinstance(body.get("error_description"), str):
                    detail["description"] = body["error_description"]
                return detail
        return {"code": "unknown_error"}


class GitHubAdapter(DefaultAdapter):
    """GitHub OAuth quirks:
    - Token exchange needs `Accept: application/json` (else GitHub returns form-encoded).
    - Token exchange returns HTTP 200 with `{error, error_description}` on failure.
    """

    def before_token_exchange(self, req: Dict[str, Any]) -> Dict[str, Any]:
        out = copy.deepcopy(req)
        out["headers"] = {**req["headers"], "Accept": "application/json"}
        return out

    def is_error_response(self, status: int, body: Any) -> bool:
        if super().is_error_response(status, body):
            return True
        if body and isinstance(body, dict) and isinstance(body.get("error"), str):
            return True
        return False


class SlackAdapter(DefaultAdapter):
    """Slack API quirks:
    - Every endpoint returns HTTP 200; errors signaled by `{ok: false, error: "..."}`.
    - Token extraction for `authed_user.*` is handled by slack.yaml
      `token_response_mapping`, NOT here (invariant #10).
    """

    def is_error_response(self, status: int, body: Any) -> bool:
        if super().is_error_response(status, body):
            return True
        if body and isinstance(body, dict) and "ok" in body:
            return body["ok"] is False
        return False

    def extract_error_detail(self, body: Any) -> Dict[str, Any]:
        if body and isinstance(body, dict) and isinstance(body.get("error"), str):
            return {"code": body["error"]}
        return super().extract_error_detail(body)


_adapters: Dict[str, DefaultAdapter] = {
    "github": GitHubAdapter(),
    "slack": SlackAdapter(),
}
_fallback: DefaultAdapter = DefaultAdapter()


def get_adapter(provider_id: str) -> DefaultAdapter:
    """Return the adapter singleton for *provider_id*.

    Providers without quirks (google, gmail, google-calendar, unknowns) get
    the DefaultAdapter.
    """
    return _adapters.get(provider_id, _fallback)
