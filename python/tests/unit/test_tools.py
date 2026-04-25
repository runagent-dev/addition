"""Unit tests for the tool engine — uses a stub async proxy, no DB."""
from __future__ import annotations

from pathlib import Path
from typing import Any, Dict
from unittest.mock import AsyncMock

import pytest

from runagent_addition.errors import ToolNotFoundError, ToolValidationError
from runagent_addition.providers import load_providers
from runagent_addition.toolkits import load_toolkits
from runagent_addition.tools import execute_tool

SHARED_PROVIDERS = Path(__file__).parents[3] / "shared" / "providers"
SHARED_TOOLKITS = Path(__file__).parents[3] / "shared" / "toolkits"


@pytest.fixture(scope="module")
def toolkits():
    providers = load_providers(str(SHARED_PROVIDERS))
    return load_toolkits(str(SHARED_TOOLKITS), providers)


def mk_proxy(status: int = 200, body: Any = None) -> AsyncMock:
    proxy = AsyncMock(return_value={"status": status, "body": body or {}})
    return proxy


# ── Param validation ──────────────────────────────────────────────────────────


class TestParamValidation:
    async def test_missing_required_param_raises_tool_validation_error(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolValidationError) as exc_info:
            await execute_tool(
                {"user_id": "u", "provider_id": "github", "tool_id": "create_issue",
                 "params": {"owner": "o"}},
                {"toolkits": toolkits, "proxy": proxy},
            )
        err = exc_info.value
        assert "repo" in err.missing
        assert "title" in err.missing
        assert "repo" in str(err)
        assert "title" in str(err)

    async def test_wrong_type_string_for_integer(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolValidationError, match="maxResults"):
            await execute_tool(
                {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages",
                 "params": {"maxResults": "not a number"}},
                {"toolkits": toolkits, "proxy": proxy},
            )

    async def test_non_integer_float_rejected(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolValidationError):
            await execute_tool(
                {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages",
                 "params": {"maxResults": 1.5}},
                {"toolkits": toolkits, "proxy": proxy},
            )

    async def test_string_list_with_non_string_element_rejected(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolValidationError, match="labelIds"):
            await execute_tool(
                {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages",
                 "params": {"labelIds": ["INBOX", 42]}},
                {"toolkits": toolkits, "proxy": proxy},
            )

    async def test_enum_invalid_value_rejected(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolValidationError, match="format"):
            await execute_tool(
                {"user_id": "u", "provider_id": "gmail", "tool_id": "get_message",
                 "params": {"messageId": "abc", "format": "invalid"}},
                {"toolkits": toolkits, "proxy": proxy},
            )

    async def test_enum_valid_value_accepted(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "gmail", "tool_id": "get_message",
             "params": {"messageId": "abc", "format": "metadata"}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        proxy.assert_called_once()

    async def test_defaults_applied_for_omitted_optional_params(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages",
             "params": {}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        call_req = proxy.call_args[0][0]
        assert call_req["params"]["maxResults"] == "10"


# ── Path interpolation ────────────────────────────────────────────────────────


class TestPathInterpolation:
    async def test_interpolates_path_params_and_removes_from_body(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "github", "tool_id": "create_issue",
             "params": {"owner": "myorg", "repo": "myrepo", "title": "Bug", "body": "details"}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert req["path"] == "/repos/myorg/myrepo/issues"
        assert "owner" not in req.get("body", {})
        assert "repo" not in req.get("body", {})
        assert req["body"]["title"] == "Bug"
        assert req["body"]["body"] == "details"

    async def test_path_params_uri_encoded(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "github", "tool_id": "create_issue",
             "params": {"owner": "my org", "repo": "my/repo", "title": "t"}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert "my org" not in req["path"]
        assert "my/repo" not in req["path"]


# ── GET vs POST routing ───────────────────────────────────────────────────────


class TestDispatchRouting:
    async def test_get_params_become_query_string(self, toolkits):
        proxy = mk_proxy(body={"messages": []})
        await execute_tool(
            {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages",
             "params": {"maxResults": 5}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert req["method"] == "GET"
        assert "params" in req
        assert req["params"]["maxResults"] == "5"
        assert "body" not in req

    async def test_post_params_become_json_body(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "github", "tool_id": "create_issue",
             "params": {"owner": "o", "repo": "r", "title": "t"}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert req["method"] == "POST"
        assert req["body"]["title"] == "t"
        assert "params" not in req


# ── Transform ─────────────────────────────────────────────────────────────────


class TestTransform:
    async def test_send_message_result_has_raw_base64url(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "gmail", "tool_id": "send_message",
             "params": {"to": "a@x.com", "subject": "hi", "body": "hello"}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert "raw" in req["body"]
        raw = req["body"]["raw"]
        assert "+" not in raw
        assert "/" not in raw
        assert "=" not in raw

    async def test_unknown_transform_raises_tool_validation_error(self, toolkits):
        fake_toolkits = {
            "fake": {
                "provider": "fake",
                "name": "Fake",
                "description": "x",
                "tools": [{
                    "id": "fake_tool", "name": "Fake", "description": "x",
                    "operation": "POST /fake", "transform": "no-such:transform",
                    "params": [], "pagination": None, "response": None,
                    "provider": "fake",
                }],
            }
        }
        proxy = mk_proxy()
        with pytest.raises(ToolValidationError):
            await execute_tool(
                {"user_id": "u", "provider_id": "fake", "tool_id": "fake_tool", "params": {}},
                {"toolkits": fake_toolkits, "proxy": proxy},
            )
        proxy.assert_not_called()

    async def test_create_event_body_transform(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "google-calendar", "tool_id": "create_event",
             "params": {"summary": "Meeting",
                        "startDateTime": "2024-06-15T09:00:00Z",
                        "endDateTime": "2024-06-15T10:00:00Z"}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert req["body"]["start"]["dateTime"] == "2024-06-15T09:00:00Z"
        assert req["body"]["end"]["dateTime"] == "2024-06-15T10:00:00Z"

    async def test_update_event_body_event_id_not_in_body(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "google-calendar", "tool_id": "update_event",
             "params": {"eventId": "evt-1", "summary": "renamed"}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert req["path"] == "/calendars/primary/events/evt-1"
        assert "eventId" not in req["body"]
        assert req["body"]["summary"] == "renamed"


# ── Dispatch shape ────────────────────────────────────────────────────────────


class TestDispatchShape:
    async def test_passes_user_provider_connection_id_to_proxy(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "user-42", "provider_id": "gmail", "tool_id": "list_messages",
             "params": {}, "connection_id": "conn-abc"},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert req["user_id"] == "user-42"
        assert req["provider_id"] == "gmail"
        assert req["connection_id"] == "conn-abc"

    async def test_connection_id_omitted_when_not_provided(self, toolkits):
        proxy = mk_proxy()
        await execute_tool(
            {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages", "params": {}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        req = proxy.call_args[0][0]
        assert "connection_id" not in req

    async def test_returns_result_reflecting_proxy_response(self, toolkits):
        proxy = mk_proxy(status=200, body={"messages": [{"id": "m1"}]})
        result = await execute_tool(
            {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages", "params": {}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        assert result["success"] is True
        assert result["status"] == 200
        assert result["data"] == {"messages": [{"id": "m1"}]}

    async def test_non_2xx_produces_success_false(self, toolkits):
        proxy = mk_proxy(status=500, body={"error": "server exploded"})
        result = await execute_tool(
            {"user_id": "u", "provider_id": "gmail", "tool_id": "list_messages", "params": {}},
            {"toolkits": toolkits, "proxy": proxy},
        )
        assert result["success"] is False
        assert result["status"] == 500


# ── Errors ────────────────────────────────────────────────────────────────────


class TestErrors:
    async def test_unknown_tool_raises_tool_not_found(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolNotFoundError):
            await execute_tool(
                {"user_id": "u", "provider_id": "gmail", "tool_id": "bogus_tool", "params": {}},
                {"toolkits": toolkits, "proxy": proxy},
            )

    async def test_unknown_provider_raises_tool_not_found(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolNotFoundError):
            await execute_tool(
                {"user_id": "u", "provider_id": "no-such", "tool_id": "anything", "params": {}},
                {"toolkits": toolkits, "proxy": proxy},
            )

    async def test_validation_error_never_reaches_proxy(self, toolkits):
        proxy = mk_proxy()
        with pytest.raises(ToolValidationError):
            await execute_tool(
                {"user_id": "u", "provider_id": "github", "tool_id": "create_issue",
                 "params": {"owner": "o"}},
                {"toolkits": toolkits, "proxy": proxy},
            )
        proxy.assert_not_called()
