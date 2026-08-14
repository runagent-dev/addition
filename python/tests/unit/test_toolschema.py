"""M14 (Python track) — tool-schema projection.

Mirrors ts/test/unit/toolschema.test.ts and drives the SAME golden fixture
shared/test-fixtures/tool_schema_export.yaml, so the TS and Python SDKs project
the toolkit catalog into byte-identical MCP tool descriptors.
"""
from pathlib import Path

import pytest
import yaml

from runagent_addition.errors import AdditionError
from runagent_addition.providers import load_providers
from runagent_addition.toolkits import get_tool_definition, load_toolkits
from runagent_addition.toolschema import (
    mcp_tool_name,
    params_to_json_schema,
    to_mcp_tool,
    tool_catalog,
)

SHARED = Path(__file__).parents[3] / "shared"
SHARED_TOOLKITS = SHARED / "toolkits"
SHARED_PROVIDERS = SHARED / "providers"
FIXTURE = SHARED / "test-fixtures" / "tool_schema_export.yaml"


@pytest.fixture(scope="module")
def toolkits():
    providers = load_providers(str(SHARED_PROVIDERS))
    return load_toolkits(str(SHARED_TOOLKITS), providers)


@pytest.fixture(scope="module")
def golden():
    return yaml.safe_load(FIXTURE.read_text())["mcp_tools"]


class TestParamsToJsonSchema:
    def test_empty_params(self):
        assert params_to_json_schema([]) == {
            "type": "object",
            "properties": {},
            "required": [],
            "additionalProperties": False,
        }

    def test_maps_each_type(self):
        params = [
            {"name": "s", "type": "string", "required": False, "description": "a string"},
            {"name": "i", "type": "integer", "required": False, "description": "an int"},
            {"name": "b", "type": "boolean", "required": False, "description": "a bool"},
            {"name": "arr", "type": "string[]", "required": False, "description": "a list"},
            {"name": "o", "type": "object", "required": False, "description": "an object"},
        ]
        assert params_to_json_schema(params)["properties"] == {
            "s": {"type": "string", "description": "a string"},
            "i": {"type": "integer", "description": "an int"},
            "b": {"type": "boolean", "description": "a bool"},
            "arr": {"type": "array", "items": {"type": "string"}, "description": "a list"},
            "o": {"type": "object", "description": "an object"},
        }

    def test_copies_enum_and_default(self):
        params = [
            {"name": "plain", "type": "string", "required": False, "description": "no extras"},
            {"name": "mode", "type": "string", "required": False, "description": "with extras",
             "enum": ["a", "b"], "default": "a"},
        ]
        props = params_to_json_schema(params)["properties"]
        assert props["plain"] == {"type": "string", "description": "no extras"}
        assert props["mode"] == {
            "type": "string", "description": "with extras", "enum": ["a", "b"], "default": "a",
        }

    def test_required_only_in_declaration_order(self):
        params = [
            {"name": "first", "type": "string", "required": True, "description": ""},
            {"name": "skip", "type": "string", "required": False, "description": ""},
            {"name": "second", "type": "string", "required": True, "description": ""},
        ]
        assert params_to_json_schema(params)["required"] == ["first", "second"]

    def test_path_params_appear_in_schema(self, toolkits):
        tool = get_tool_definition(toolkits, "gmail", "get_message")
        schema = params_to_json_schema(tool["params"])
        assert schema["properties"]["messageId"] == {
            "type": "string", "description": "The ID of the message to retrieve",
        }
        assert "messageId" in schema["required"]


class TestPurity:
    def test_enum_not_aliased(self, toolkits):
        tool = get_tool_definition(toolkits, "gmail", "get_message")
        fmt = next(p for p in tool["params"] if p["name"] == "format")
        original = list(fmt["enum"])
        schema = params_to_json_schema(tool["params"])
        schema["properties"]["format"]["enum"].append("MUTATED")
        assert fmt["enum"] == original

    def test_default_not_aliased(self):
        params = [{"name": "tags", "type": "string[]", "required": False,
                   "description": "", "default": ["a", "b"]}]
        schema = params_to_json_schema(params)
        schema["properties"]["tags"]["default"].append("MUTATED")
        assert params[0]["default"] == ["a", "b"]


class TestParseOperationGuard:
    def test_tool_catalog_raises_on_malformed_operation(self):
        bad = {
            "x": {
                "provider": "x", "name": "X", "description": "",
                "tools": [{
                    "id": "t", "name": "T", "description": "", "provider": "x",
                    "operation": "GETnoSpace", "params": [],
                    "transform": None, "pagination": None, "response": None,
                }],
            }
        }
        with pytest.raises(AdditionError):
            tool_catalog(bad)


class TestMcpToolName:
    def test_joins_with_double_underscore(self):
        assert mcp_tool_name("gmail", "send_message") == "gmail__send_message"

    def test_reversible(self):
        provider, tool_id = mcp_tool_name("github", "create_issue").split("__")
        assert [provider, tool_id] == ["github", "create_issue"]


class TestGoldenFixture:
    def test_has_entries(self, golden):
        assert len(golden) > 0

    def test_to_mcp_tool_matches_golden(self, toolkits, golden):
        for e in golden:
            tool = get_tool_definition(toolkits, e["provider"], e["toolId"])
            assert to_mcp_tool(e["provider"], tool) == {
                "name": e["name"],
                "description": e["description"],
                "inputSchema": e["inputSchema"],
            }

    def test_tool_catalog_matches_golden(self, toolkits, golden):
        catalog = tool_catalog(toolkits)
        by_name = {c["mcpName"]: c for c in catalog}
        for e in golden:
            assert by_name[e["name"]] == {
                "provider": e["provider"],
                "toolId": e["toolId"],
                "mcpName": e["name"],
                "description": e["description"],
                "inputSchema": e["inputSchema"],
                "method": e["method"],
                "path": e["path"],
            }

    def test_tool_catalog_covers_every_tool(self, toolkits):
        total = sum(len(tk["tools"]) for tk in toolkits.values())
        assert len(tool_catalog(toolkits)) == total
