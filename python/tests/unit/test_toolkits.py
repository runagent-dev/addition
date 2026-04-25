import tempfile
from pathlib import Path

import pytest

from runagent_addition.errors import AdditionError, ToolNotFoundError
from runagent_addition.providers import load_providers
from runagent_addition.toolkits import get_tool_definition, get_tool_definitions, load_toolkits

SHARED_TOOLKITS = Path(__file__).parents[3] / "shared" / "toolkits"
SHARED_PROVIDERS = Path(__file__).parents[3] / "shared" / "providers"


def _write(dir_: str, filename: str, content: str) -> None:
    Path(dir_).joinpath(filename).write_text(content)


class TestLoading:
    def setup_method(self):
        self.providers = load_providers(str(SHARED_PROVIDERS))
        self.toolkits = load_toolkits(str(SHARED_TOOLKITS), self.providers)

    def test_loads_all_4_toolkits(self):
        ids = sorted(self.toolkits.keys())
        assert ids == ["github", "gmail", "google-calendar", "slack"]

    def test_ignores_template_yaml(self):
        ids = list(self.toolkits.keys())
        assert "_template" not in ids
        assert "" not in ids
        assert len(ids) == 4

    def test_toolkit_metadata(self):
        assert self.toolkits["gmail"]["name"] == "Gmail"
        assert "email" in self.toolkits["gmail"]["description"].lower()
        assert self.toolkits["github"]["name"] == "GitHub"


class TestGetToolDefinitions:
    def setup_method(self):
        providers = load_providers(str(SHARED_PROVIDERS))
        self.toolkits = load_toolkits(str(SHARED_TOOLKITS), providers)

    def test_gmail_has_5_tools(self):
        tools = get_tool_definitions(self.toolkits, "gmail")
        assert len(tools) == 5
        ids = sorted(t["id"] for t in tools)
        assert ids == ["get_message", "list_messages", "modify_message", "search_messages", "send_message"]

    def test_google_calendar_has_5_tools(self):
        tools = get_tool_definitions(self.toolkits, "google-calendar")
        assert len(tools) == 5
        ids = sorted(t["id"] for t in tools)
        assert ids == ["create_event", "delete_event", "get_event", "list_events", "update_event"]

    def test_github_has_5_tools(self):
        tools = get_tool_definitions(self.toolkits, "github")
        assert len(tools) == 5

    def test_slack_has_3_tools(self):
        tools = get_tool_definitions(self.toolkits, "slack")
        assert len(tools) == 3

    def test_unknown_toolkit_raises(self):
        with pytest.raises(ToolNotFoundError):
            get_tool_definitions(self.toolkits, "nonexistent")

    def test_every_tool_carries_provider_id(self):
        tools = get_tool_definitions(self.toolkits, "gmail")
        for t in tools:
            assert t["provider"] == "gmail"


class TestGetToolDefinition:
    def setup_method(self):
        providers = load_providers(str(SHARED_PROVIDERS))
        self.toolkits = load_toolkits(str(SHARED_TOOLKITS), providers)

    def test_returns_single_tool_by_id(self):
        send = get_tool_definition(self.toolkits, "gmail", "send_message")
        assert send["id"] == "send_message"
        assert send["name"] == "Send email"
        assert send["provider"] == "gmail"

    def test_unknown_provider_raises(self):
        with pytest.raises(ToolNotFoundError):
            get_tool_definition(self.toolkits, "nope", "anything")

    def test_unknown_tool_raises(self):
        with pytest.raises(ToolNotFoundError):
            get_tool_definition(self.toolkits, "gmail", "bogus")


class TestToolDefinitionShape:
    def setup_method(self):
        providers = load_providers(str(SHARED_PROVIDERS))
        self.toolkits = load_toolkits(str(SHARED_TOOLKITS), providers)

    def test_send_message_param_schema(self):
        send = get_tool_definition(self.toolkits, "gmail", "send_message")
        by_name = {p["name"]: p for p in send["params"]}
        assert by_name["to"]["type"] == "string"
        assert by_name["to"]["required"] is True
        assert "recipient" in by_name["to"]["description"].lower()
        assert by_name["subject"]["required"] is True
        assert by_name["body"]["required"] is True
        assert by_name["cc"]["required"] is False
        assert by_name["bcc"]["required"] is False

    def test_get_message_enum_and_default(self):
        get = get_tool_definition(self.toolkits, "gmail", "get_message")
        fmt = next(p for p in get["params"] if p["name"] == "format")
        assert fmt["enum"] == ["minimal", "full", "raw", "metadata"]
        assert fmt["default"] == "full"

    def test_list_messages_integer_with_default_and_string_list_param(self):
        lst = get_tool_definition(self.toolkits, "gmail", "list_messages")
        by_name = {p["name"]: p for p in lst["params"]}
        assert by_name["maxResults"]["type"] == "integer"
        assert by_name["maxResults"]["default"] == 10
        assert by_name["labelIds"]["type"] == "string[]"
        assert by_name["labelIds"]["required"] is False

    def test_operation_field(self):
        send = get_tool_definition(self.toolkits, "gmail", "send_message")
        assert send["operation"] == "POST /gmail/v1/users/me/messages/send"
        lst = get_tool_definition(self.toolkits, "gmail", "list_messages")
        assert lst["operation"] == "GET /gmail/v1/users/me/messages"

    def test_transform_populated_when_defined(self):
        send = get_tool_definition(self.toolkits, "gmail", "send_message")
        assert send["transform"] == "gmail:rfc2822"

    def test_transform_null_when_not_defined(self):
        lst = get_tool_definition(self.toolkits, "gmail", "list_messages")
        assert lst["transform"] is None

    def test_github_create_issue_path_params(self):
        ci = get_tool_definition(self.toolkits, "github", "create_issue")
        assert ci["operation"] == "POST /repos/{owner}/{repo}/issues"
        by_name = {p["name"]: p for p in ci["params"]}
        assert by_name["owner"]["required"] is True
        assert by_name["repo"]["required"] is True
        assert by_name["title"]["required"] is True
        assert by_name["body"]["required"] is False


class TestPaginationCursorConvention:
    def setup_method(self):
        providers = load_providers(str(SHARED_PROVIDERS))
        self.toolkits = load_toolkits(str(SHARED_TOOLKITS), providers)

    def test_gmail_list_messages_top_level_cursor(self):
        lst = get_tool_definition(self.toolkits, "gmail", "list_messages")
        assert lst["pagination"] is not None
        assert lst["pagination"]["cursor_response_field"] == "nextPageToken"
        assert "." not in lst["pagination"]["cursor_response_field"]

    def test_slack_list_channels_dotpath_cursor(self):
        lst = get_tool_definition(self.toolkits, "slack", "list_channels")
        assert lst["pagination"] is not None
        assert lst["pagination"]["cursor_response_field"] == "response_metadata.next_cursor"
        assert "." in lst["pagination"]["cursor_response_field"]

    def test_slack_list_messages_dotpath_cursor(self):
        lst = get_tool_definition(self.toolkits, "slack", "list_messages")
        assert lst["pagination"]["cursor_response_field"] == "response_metadata.next_cursor"

    def test_github_list_repos_page_offset_null_cursor(self):
        lst = get_tool_definition(self.toolkits, "github", "list_repos")
        assert lst["pagination"]["type"] == "page"
        assert lst["pagination"]["cursor_response_field"] is None


class TestCrossReferenceToProviders:
    def setup_method(self):
        self.providers = load_providers(str(SHARED_PROVIDERS))
        self.toolkits = load_toolkits(str(SHARED_TOOLKITS), self.providers)

    def test_all_toolkit_provider_ids_are_loaded(self):
        for pid in self.toolkits:
            assert pid in self.providers

    def test_rejects_toolkit_with_unknown_provider(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "phantom.yaml",
                'provider: "phantom-provider-no-yaml"\n'
                'name: "Phantom"\n'
                'description: "No backing provider"\n'
                "tools:\n"
                "  - id: noop\n"
                '    name: "Noop"\n'
                '    description: "does nothing"\n'
                '    operation: "GET /noop"\n'
                "    params: []\n",
            )
            with pytest.raises(Exception, match="phantom-provider-no-yaml"):
                load_toolkits(tmp, self.providers)


class TestShapeValidation:
    def setup_method(self):
        self.providers = load_providers(str(SHARED_PROVIDERS))

    def test_rejects_toolkit_missing_provider_field(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(tmp, "bad.yaml", 'name: "Bad"\ndescription: "no provider"\ntools: []\n')
            with pytest.raises(AdditionError):
                load_toolkits(tmp, self.providers)

    def test_rejects_tool_missing_operation(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "bad.yaml",
                'provider: "gmail"\nname: "Broken"\ndescription: "x"\n'
                "tools:\n  - id: foo\n    name: Foo\n    description: x\n    params: []\n",
            )
            with pytest.raises(Exception, match="operation"):
                load_toolkits(tmp, self.providers)

    def test_rejects_param_with_unknown_type(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "bad.yaml",
                'provider: "gmail"\nname: "Bad"\ndescription: "x"\n'
                "tools:\n"
                "  - id: foo\n    name: Foo\n    description: x\n"
                '    operation: "GET /foo"\n'
                "    params:\n"
                "      - name: q\n        type: blobstring\n"
                "        required: true\n        description: bad\n",
            )
            with pytest.raises(Exception, match="blobstring|type"):
                load_toolkits(tmp, self.providers)

    def test_rejects_enum_values_wrong_type(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "bad.yaml",
                'provider: "gmail"\nname: "Bad"\ndescription: "x"\n'
                "tools:\n"
                "  - id: foo\n    name: Foo\n    description: x\n"
                '    operation: "GET /foo"\n'
                "    params:\n"
                "      - name: n\n        type: integer\n"
                "        required: true\n"
                '        enum: ["one", "two"]\n'
                "        description: should be ints\n",
            )
            with pytest.raises(Exception, match="enum|integer"):
                load_toolkits(tmp, self.providers)

    def test_rejects_enum_on_boolean_param(self):
        # Note: "on" is a YAML boolean alias — use "flag" to avoid misparse.
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "bad.yaml",
                'provider: "gmail"\nname: "Bad"\ndescription: "x"\n'
                "tools:\n"
                "  - id: foo\n    name: Foo\n    description: x\n"
                '    operation: "GET /foo"\n'
                "    params:\n"
                "      - name: flag\n        type: boolean\n"
                "        required: true\n"
                "        enum: [true, false]\n"
                "        description: nonsensical\n",
            )
            with pytest.raises(Exception, match="enum"):
                load_toolkits(tmp, self.providers)

    def test_accepts_integer_enum(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "good.yaml",
                'provider: "gmail"\nname: "Good"\ndescription: "x"\n'
                "tools:\n"
                "  - id: foo\n    name: Foo\n    description: x\n"
                '    operation: "GET /foo"\n'
                "    params:\n"
                "      - name: n\n        type: integer\n"
                "        required: true\n"
                "        enum: [1, 2, 3]\n"
                "        description: valid\n",
            )
            reg = load_toolkits(tmp, self.providers)
            assert reg["gmail"]["tools"][0]["params"][0]["enum"] == [1, 2, 3]

    def test_rejects_non_string_cursor_response_field(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "bad.yaml",
                'provider: "gmail"\nname: "Bad"\ndescription: "x"\n'
                "tools:\n"
                "  - id: foo\n    name: Foo\n    description: x\n"
                '    operation: "GET /foo"\n'
                "    params: []\n"
                "    pagination:\n"
                '      type: "cursor"\n'
                '      cursor_param: "pageToken"\n'
                "      cursor_response_field: 42\n",
            )
            with pytest.raises(Exception, match="cursor_response_field"):
                load_toolkits(tmp, self.providers)

    def test_rejects_non_dict_response_field(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "bad.yaml",
                'provider: "gmail"\nname: "Bad"\ndescription: "x"\n'
                "tools:\n"
                "  - id: foo\n    name: Foo\n    description: x\n"
                '    operation: "GET /foo"\n'
                "    params: []\n"
                '    response: "string-not-object"\n',
            )
            with pytest.raises(Exception, match="response"):
                load_toolkits(tmp, self.providers)

    def test_rejects_duplicate_tool_ids_within_toolkit(self):
        with tempfile.TemporaryDirectory() as tmp:
            _write(
                tmp,
                "dup.yaml",
                'provider: "gmail"\nname: "Dup"\ndescription: "x"\n'
                "tools:\n"
                "  - id: foo\n    name: Foo\n    description: x\n"
                '    operation: "GET /foo"\n    params: []\n'
                "  - id: foo\n    name: Foo2\n    description: x\n"
                '    operation: "GET /foo2"\n    params: []\n',
            )
            with pytest.raises(Exception, match="duplicate.*foo|foo.*duplicate"):
                load_toolkits(tmp, self.providers)
