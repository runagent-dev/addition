import pytest

from runagent_addition.adapters import DefaultAdapter, GitHubAdapter, SlackAdapter, get_adapter


def _mk_req(**overrides):
    req = {
        "url": "https://example.com/token",
        "method": "POST",
        "headers": {"Content-Type": "application/x-www-form-urlencoded"},
        "body": {"grant_type": "authorization_code", "code": "abc"},
    }
    req.update(overrides)
    return req


class TestInvariant10Scope:
    """Adapters must NOT implement parseTokenResponse — token extraction is YAML + engine."""

    def test_default_adapter_has_no_parse_token_response(self):
        assert not hasattr(DefaultAdapter(), "parse_token_response")

    def test_github_adapter_has_no_parse_token_response(self):
        assert not hasattr(GitHubAdapter(), "parse_token_response")

    def test_slack_adapter_has_no_parse_token_response(self):
        assert not hasattr(SlackAdapter(), "parse_token_response")


class TestDefaultAdapter:
    def setup_method(self):
        self.a = DefaultAdapter()

    def test_is_error_response_true_for_500(self):
        assert self.a.is_error_response(500, {}) is True

    def test_is_error_response_true_for_400(self):
        assert self.a.is_error_response(400, {"error": "bad_request"}) is True

    def test_is_error_response_false_for_200(self):
        assert self.a.is_error_response(200, {}) is False

    def test_is_error_response_false_for_201(self):
        assert self.a.is_error_response(201, {"id": "abc"}) is False

    def test_extract_error_detail_reads_oauth2_fields(self):
        detail = self.a.extract_error_detail({
            "error": "invalid_grant",
            "error_description": "Authorization code expired",
        })
        assert detail["code"] == "invalid_grant"
        assert detail["description"] == "Authorization code expired"

    def test_extract_error_detail_handles_missing_description(self):
        detail = self.a.extract_error_detail({"error": "invalid_grant"})
        assert detail["code"] == "invalid_grant"
        assert "description" not in detail

    def test_extract_error_detail_falls_back_for_non_oauth_bodies(self):
        assert self.a.extract_error_detail({})["code"] == "unknown_error"
        assert self.a.extract_error_detail(None)["code"] == "unknown_error"
        assert self.a.extract_error_detail("string body")["code"] == "unknown_error"

    def test_before_token_exchange_is_noop(self):
        req = _mk_req()
        assert self.a.before_token_exchange(req) == req


class TestGitHubAdapter:
    def setup_method(self):
        self.a = GitHubAdapter()

    def test_before_token_exchange_adds_accept_json(self):
        req = _mk_req()
        out = self.a.before_token_exchange(req)
        assert out["headers"]["Accept"] == "application/json"

    def test_before_token_exchange_preserves_existing_headers(self):
        req = _mk_req()
        out = self.a.before_token_exchange(req)
        assert out["headers"]["Content-Type"] == "application/x-www-form-urlencoded"

    def test_before_token_exchange_does_not_mutate_input(self):
        import copy
        req = _mk_req()
        snapshot = copy.deepcopy(req)
        self.a.before_token_exchange(req)
        assert req == snapshot

    def test_is_error_response_true_for_200_with_error_body(self):
        assert self.a.is_error_response(200, {
            "error": "bad_verification_code",
            "error_description": "The code passed is incorrect or expired.",
        }) is True

    def test_is_error_response_false_for_200_healthy_body(self):
        assert self.a.is_error_response(200, {"access_token": "gho_xxx", "token_type": "bearer"}) is False

    def test_is_error_response_true_for_500(self):
        assert self.a.is_error_response(500, {}) is True

    def test_extract_error_detail_reads_error_fields(self):
        detail = self.a.extract_error_detail({
            "error": "bad_verification_code",
            "error_description": "The code passed is incorrect or expired.",
        })
        assert detail["code"] == "bad_verification_code"
        assert detail["description"] == "The code passed is incorrect or expired."


class TestSlackAdapter:
    def setup_method(self):
        self.a = SlackAdapter()

    def test_is_error_response_true_for_200_ok_false(self):
        assert self.a.is_error_response(200, {"ok": False, "error": "invalid_auth"}) is True

    def test_is_error_response_false_for_200_ok_true(self):
        assert self.a.is_error_response(200, {"ok": True, "channel": "C123", "ts": "1.2"}) is False

    def test_is_error_response_true_for_500(self):
        assert self.a.is_error_response(500, {}) is True

    def test_is_error_response_ignores_bodies_without_ok(self):
        assert self.a.is_error_response(200, {"some": "payload"}) is False

    def test_extract_error_detail_reads_top_level_error(self):
        detail = self.a.extract_error_detail({"ok": False, "error": "invalid_auth"})
        assert detail["code"] == "invalid_auth"

    def test_before_token_exchange_is_noop(self):
        req = _mk_req()
        assert self.a.before_token_exchange(req) == req


class TestAdapterRegistry:
    def test_github_returns_github_adapter(self):
        assert isinstance(get_adapter("github"), GitHubAdapter)

    def test_slack_returns_slack_adapter(self):
        assert isinstance(get_adapter("slack"), SlackAdapter)

    def test_google_returns_default_adapter(self):
        assert isinstance(get_adapter("google"), DefaultAdapter)

    def test_gmail_returns_default_adapter(self):
        assert isinstance(get_adapter("gmail"), DefaultAdapter)

    def test_google_calendar_returns_default_adapter(self):
        assert isinstance(get_adapter("google-calendar"), DefaultAdapter)

    def test_unknown_provider_returns_default_adapter(self):
        assert isinstance(get_adapter("unknown-provider"), DefaultAdapter)

    def test_registry_returns_stable_singletons(self):
        assert get_adapter("github") is get_adapter("github")
        assert get_adapter("slack") is get_adapter("slack")
