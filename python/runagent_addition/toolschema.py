"""Tool-schema projection (M14). Pure mappers from the shared toolkit catalog to
JSON-Schema input contracts and MCP tool descriptors. The REST endpoint and MCP
server generators build on these — no projection re-derives an input schema on
its own. Mirrors ts/src/toolschema.ts; output must match byte-for-byte.
"""
from __future__ import annotations

import copy
from typing import Any

from .errors import AdditionError

_MCP_NAME_SEP = "__"


def mcp_tool_name(provider: str, tool_id: str) -> str:
    """MCP tool name for a (provider, tool) pair. Reversible when neither contains ``__``."""
    return f"{provider}{_MCP_NAME_SEP}{tool_id}"


def _base_schema(type_: str) -> dict:
    """A fresh base schema per call — never share nested objects (e.g. ``items``) across params."""
    if type_ == "string":
        return {"type": "string"}
    if type_ == "integer":
        return {"type": "integer"}
    if type_ == "boolean":
        return {"type": "boolean"}
    if type_ == "string[]":
        return {"type": "array", "items": {"type": "string"}}
    if type_ == "object":
        return {"type": "object"}
    raise AdditionError(f"unknown param type '{type_}'")


def _param_to_schema(param: dict) -> dict:
    schema = _base_schema(param["type"])
    schema["description"] = param["description"]
    # Never alias the input catalog into the output — copy the enum and deep-copy
    # the default (which may be an array/object) so the projection stays pure.
    if "enum" in param:
        schema["enum"] = list(param["enum"])
    if "default" in param:
        schema["default"] = copy.deepcopy(param["default"])
    return schema


def params_to_json_schema(params: list[dict]) -> dict:
    """JSON Schema describing a tool's caller inputs. Every declared param appears —
    including path params interpolated into the URL at execution — because the caller
    has to supply them."""
    properties: dict[str, Any] = {}
    required: list[str] = []
    for param in params:
        properties[param["name"]] = _param_to_schema(param)
        if param["required"]:
            required.append(param["name"])
    return {
        "type": "object",
        "properties": properties,
        "required": required,
        "additionalProperties": False,
    }


def to_mcp_tool(provider: str, tool: dict) -> dict:
    """MCP tool descriptor for a tool: name, description, and JSON-Schema inputs."""
    return {
        "name": mcp_tool_name(provider, tool["id"]),
        "description": tool["description"],
        "inputSchema": params_to_json_schema(tool["params"]),
    }


def _parse_operation(operation: str) -> tuple[str, str]:
    idx = operation.find(" ")
    if idx == -1:
        raise AdditionError(
            f"Malformed tool operation '{operation}': expected \"METHOD /path\""
        )
    return operation[:idx], operation[idx + 1:]


def tool_catalog(registry: dict) -> list[dict]:
    """Flat, normalized view of every tool across every toolkit — the projection base."""
    entries: list[dict] = []
    for toolkit in registry.values():
        for tool in toolkit["tools"]:
            method, path = _parse_operation(tool["operation"])
            entries.append({
                "provider": toolkit["provider"],
                "toolId": tool["id"],
                "mcpName": mcp_tool_name(toolkit["provider"], tool["id"]),
                "description": tool["description"],
                "inputSchema": params_to_json_schema(tool["params"]),
                "method": method,
                "path": path,
            })
    return entries
