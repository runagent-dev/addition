import tempfile
from pathlib import Path

import pytest

from runagent_addition.errors import AdditionError, ProviderNotFoundError
from runagent_addition.providers import get_provider, load_providers

SHARED_PROVIDERS = Path(__file__).parents[3] / "shared" / "providers"

# ── A fully-specified oauth2 block used in synthetic fixtures ──────────────────
_OAUTH2_BLOCK = """oauth2:
  authorize_url: "https://example.com/auth"
  token_url: "https://example.com/token"
  refresh_supported: true
  pkce_supported: false
  pkce_required: false
  default_scopes: []
  scopes_delimiter: "space"
  authorize_params: {}
  token_params: {}
  token_response_format: "json"
  client_auth_method: "body"
"""

_API_BLOCK = """api:
  base_url: "https://example.com"
  auth_header: "Authorization"
  auth_prefix: "Bearer"
"""


def _write(dir_: str, filename: str, content: str) -> None:
    Path(dir_).joinpath(filename).write_text(content)


class TestLoading:
    def setup_method(self):
        self.registry = load_providers(str(SHARED_PROVIDERS))

    def test_loads_all_5_providers(self):
        ids = sorted(self.registry.keys())
        assert ids == ["github", "gmail", "google", "google-calendar", "slack"]

    def test_ignores_template_yaml(self):
        ids = list(self.registry.keys())
        assert "_template" not in ids
        assert "" not in ids
        assert len(ids) == 5

    def test_get_provider_returns_config(self):
        google = get_provider(self.registry, "google")
        assert google["id"] == "google"
        assert google["name"] == "Google"
        assert google["auth_type"] == "oauth2"
        assert google["oauth2"]["authorize_url"] == "https://accounts.google.com/o/oauth2/v2/auth"

    def test_get_provider_unknown_raises(self):
        with pytest.raises(ProviderNotFoundError):
            get_provider(self.registry, "nonexistent")


class TestExtendsResolution:
    def setup_method(self):
        self.registry = load_providers(str(SHARED_PROVIDERS))

    def test_gmail_inherits_google_authorize_and_token_url(self):
        gmail = get_provider(self.registry, "gmail")
        google = get_provider(self.registry, "google")
        assert gmail["oauth2"]["authorize_url"] == google["oauth2"]["authorize_url"]
        assert gmail["oauth2"]["token_url"] == google["oauth2"]["token_url"]

    def test_gmail_inherits_scopes_delimiter_and_authorize_params(self):
        gmail = get_provider(self.registry, "gmail")
        assert gmail["oauth2"]["scopes_delimiter"] == "space"
        assert gmail["oauth2"]["authorize_params"] == {"access_type": "offline", "prompt": "consent"}

    def test_array_override_gmail_scopes_replace_not_merge(self):
        gmail = get_provider(self.registry, "gmail")
        google = get_provider(self.registry, "google")
        gmail_scopes = gmail["oauth2"]["default_scopes"]
        assert "https://www.googleapis.com/auth/gmail.send" in gmail_scopes
        assert "https://www.googleapis.com/auth/userinfo.profile" not in gmail_scopes
        assert len(gmail_scopes) <= len(google["oauth2"]["default_scopes"]) + 10

    def test_scalar_override_gmail_base_url(self):
        gmail = get_provider(self.registry, "gmail")
        assert gmail["api"]["base_url"] == "https://gmail.googleapis.com"

    def test_child_inherits_unspecified_nested_fields(self):
        gmail = get_provider(self.registry, "gmail")
        assert gmail["api"]["auth_header"] == "Authorization"
        assert gmail["api"]["auth_prefix"] == "Bearer"

    def test_id_and_name_from_child(self):
        gmail = get_provider(self.registry, "gmail")
        assert gmail["id"] == "gmail"
        assert gmail["name"] == "Gmail"

    def test_google_calendar_inherits_oauth_overrides_base_url(self):
        cal = get_provider(self.registry, "google-calendar")
        assert cal["id"] == "google-calendar"
        assert cal["oauth2"]["authorize_url"] == "https://accounts.google.com/o/oauth2/v2/auth"
        assert cal["api"]["base_url"] == "https://www.googleapis.com/calendar/v3"

    def test_provider_without_extends_returned_as_is(self):
        github = get_provider(self.registry, "github")
        assert github["oauth2"]["refresh_supported"] is False
        assert github["oauth2"]["default_scopes"] == ["repo", "read:user", "user:email"]


class TestTopologyAndCycles:
    def test_topo_sort_loads_child_regardless_of_file_order(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "aaa.yaml",
                'id: "aaa"\nname: "Aaa"\nauth_type: "oauth2"\nextends: "zzz"\n'
                "api:\n  base_url: \"https://aaa.example.com\"\n",
            )
            _write(
                tmp,
                "zzz.yaml",
                'id: "zzz"\nname: "Zzz"\nauth_type: "oauth2"\n'
                + _OAUTH2_BLOCK
                + _API_BLOCK,
            )
            reg = load_providers(tmp)
            assert reg["aaa"]["oauth2"]["authorize_url"] == "https://example.com/auth"
            assert reg["aaa"]["api"]["base_url"] == "https://aaa.example.com"

    def test_cycle_raises_clear_error(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(tmp, "a.yaml", 'id: "a"\nname: "A"\nauth_type: "oauth2"\nextends: "b"\n')
            _write(tmp, "b.yaml", 'id: "b"\nname: "B"\nauth_type: "oauth2"\nextends: "a"\n')
            with pytest.raises((AdditionError, Exception), match="cycle"):
                load_providers(tmp)

    def test_extends_unknown_id_raises(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "orphan.yaml",
                'id: "orphan"\nname: "Orphan"\nauth_type: "oauth2"\nextends: "ghost"\n',
            )
            with pytest.raises(Exception, match="ghost"):
                load_providers(tmp)

    def test_deep_merge_3_levels(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "base.yaml",
                'id: "base"\nname: "Base"\nauth_type: "oauth2"\n'
                + _OAUTH2_BLOCK
                + "api:\n"
                '  base_url: "https://base.example.com"\n'
                '  auth_header: "Authorization"\n'
                '  auth_prefix: "Bearer"\n'
                "  rate_limit:\n"
                "    requests_per_second: 10\n"
                '    retry_after_header: "Retry-After"\n',
            )
            _write(
                tmp,
                "child.yaml",
                'id: "child"\nname: "Child"\nauth_type: "oauth2"\nextends: "base"\n'
                "api:\n  rate_limit:\n    requests_per_second: 99\n",
            )
            reg = load_providers(tmp)
            assert reg["child"]["api"]["rate_limit"]["requests_per_second"] == 99
            assert reg["child"]["api"]["rate_limit"]["retry_after_header"] == "Retry-After"
            assert reg["child"]["api"]["base_url"] == "https://base.example.com"
            assert reg["child"]["api"]["auth_header"] == "Authorization"


class TestPostMergeValidation:
    def test_throws_when_authorize_url_missing(self):
        # Use a typo so the field name doesn't match — avoids YAML-indentation
        # issues from line removal while still exercising the validation check.
        with tempfile.TemporaryDirectory() as tmp:
            broken_oauth = _OAUTH2_BLOCK.replace(
                '  authorize_url:', '  authorize_ur:'
            )
            _write(
                tmp,
                "broken.yaml",
                f'id: "broken"\nname: "Broken"\nauth_type: "oauth2"\n{broken_oauth}{_API_BLOCK}',
            )
            with pytest.raises(Exception, match="authorize_url"):
                load_providers(tmp)

    def test_throws_when_api_base_url_missing(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "nourl.yaml",
                f'id: "nourl"\nname: "No URL"\nauth_type: "oauth2"\n{_OAUTH2_BLOCK}'
                "api:\n"
                '  auth_header: "Authorization"\n'
                '  auth_prefix: "Bearer"\n',
            )
            with pytest.raises(Exception, match="base_url"):
                load_providers(tmp)

    def test_accepts_minimal_fully_specified_provider(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "ok.yaml",
                f'id: "ok"\nname: "OK"\nauth_type: "oauth2"\n{_OAUTH2_BLOCK}{_API_BLOCK}',
            )
            reg = load_providers(tmp)
            assert reg["ok"]["id"] == "ok"

    @pytest.mark.parametrize(
        "field,line",
        [
            ("refresh_supported", "refresh_supported: true"),
            ("pkce_supported", "pkce_supported: false"),
            ("pkce_required", "pkce_required: false"),
            ("scopes_delimiter", 'scopes_delimiter: "space"'),
            ("default_scopes", "default_scopes: []"),
            ("authorize_params", "authorize_params: {}"),
            ("token_params", "token_params: {}"),
            ("token_response_format", 'token_response_format: "json"'),
            ("client_auth_method", 'client_auth_method: "body"'),
        ],
    )
    def test_throws_when_oauth2_field_missing(self, field, line):
        with tempfile.TemporaryDirectory() as tmp:
            oauth_block = "\n".join(
                l for l in _OAUTH2_BLOCK.splitlines() if line not in l
            ) + "\n"
            _write(
                tmp,
                "missing.yaml",
                f'id: "missing"\nname: "Missing"\nauth_type: "oauth2"\n{oauth_block}{_API_BLOCK}',
            )
            with pytest.raises(Exception, match=field):
                load_providers(tmp)

    def test_throws_on_invalid_scopes_delimiter(self):
        with tempfile.TemporaryDirectory() as tmp:
            oauth_block = _OAUTH2_BLOCK.replace(
                'scopes_delimiter: "space"', 'scopes_delimiter: "semicolon"'
            )
            _write(
                tmp,
                "bad.yaml",
                f'id: "bad"\nname: "Bad"\nauth_type: "oauth2"\n{oauth_block}{_API_BLOCK}',
            )
            with pytest.raises(Exception, match="scopes_delimiter"):
                load_providers(tmp)

    def test_throws_when_pkce_supported_is_string_not_bool(self):
        with tempfile.TemporaryDirectory() as tmp:
            oauth_block = _OAUTH2_BLOCK.replace(
                "pkce_supported: false", 'pkce_supported: "false"'
            )
            _write(
                tmp,
                "wrong.yaml",
                f'id: "wrong"\nname: "Wrong"\nauth_type: "oauth2"\n{oauth_block}{_API_BLOCK}',
            )
            with pytest.raises(Exception, match="pkce_supported"):
                load_providers(tmp)

    def test_throws_when_child_extends_without_name(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "base.yaml",
                f'id: "the-base"\nname: "The Base"\nauth_type: "oauth2"\n{_OAUTH2_BLOCK}{_API_BLOCK}',
            )
            _write(
                tmp,
                "nameless.yaml",
                'id: "nameless"\nauth_type: "oauth2"\nextends: "the-base"\n',
            )
            with pytest.raises(Exception, match="nameless|name"):
                load_providers(tmp)


class TestTokenResponseMapping:
    def setup_method(self):
        self.registry = load_providers(str(SHARED_PROVIDERS))

    def test_google_gmail_github_have_no_mapping(self):
        for pid in ("google", "gmail", "github"):
            p = get_provider(self.registry, pid)
            assert p["oauth2"].get("token_response_mapping") is None

    def test_slack_has_nested_mapping(self):
        slack = get_provider(self.registry, "slack")
        mapping = slack["oauth2"]["token_response_mapping"]
        assert mapping == {
            "access_token": "authed_user.access_token",
            "refresh_token": "authed_user.refresh_token",
            "expires_in": "authed_user.expires_in",
        }
