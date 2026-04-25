from __future__ import annotations

import os
from typing import Any, Dict, List, Optional

import yaml

from .errors import AdditionError, ToolNotFoundError

ToolkitRegistry = Dict[str, Any]

_VALID_PARAM_TYPES = {"string", "integer", "boolean", "string[]", "object"}
_VALID_HTTP_METHODS = {"GET", "POST", "PUT", "PATCH", "DELETE"}


def _require_string(value: Any, path: str, context: str) -> str:
    if not isinstance(value, str) or value == "":
        raise AdditionError(f"{context}: missing required string field '{path}'")
    return value


def _parse_param(raw: dict, context: str) -> dict:
    name = _require_string(raw.get("name"), "name", context)
    ctx = f"{context} param '{name}'"
    type_str = _require_string(raw.get("type"), "type", ctx)
    if type_str not in _VALID_PARAM_TYPES:
        raise AdditionError(
            f"{ctx}: invalid type '{type_str}' (must be one of {', '.join(sorted(_VALID_PARAM_TYPES))})"
        )
    if not isinstance(raw.get("required"), bool):
        raise AdditionError(f"{ctx}: 'required' must be a boolean")
    description = _require_string(raw.get("description"), "description", ctx)

    out: dict = {
        "name": name,
        "type": type_str,
        "required": raw["required"],
        "description": description,
    }
    if "default" in raw:
        out["default"] = raw["default"]

    if "enum" in raw:
        enum_val = raw["enum"]
        if not isinstance(enum_val, list):
            raise AdditionError(f"{ctx}: 'enum' must be an array")
        _ok_enum_type: dict[str, Any] = {
            "string": lambda v: isinstance(v, str),
            "integer": lambda v: isinstance(v, int) and not isinstance(v, bool),
        }
        check = _ok_enum_type.get(type_str)
        if check is None:
            raise AdditionError(
                f"{ctx}: 'enum' is only allowed on 'string' or 'integer' params, not '{type_str}'"
            )
        for v in enum_val:
            if not check(v):
                raise AdditionError(
                    f"{ctx}: enum value {v!r} is not a {type_str}"
                )
        out["enum"] = enum_val

    return out


def _parse_operation(op: str, context: str) -> None:
    parts = op.split(" ", 1)
    method = parts[0]
    if method not in _VALID_HTTP_METHODS:
        raise AdditionError(
            f"{context}: operation '{op}' must start with a valid HTTP method "
            f"({'|'.join(sorted(_VALID_HTTP_METHODS))})"
        )
    if len(parts) < 2 or not parts[1].startswith("/"):
        raise AdditionError(
            f"{context}: operation '{op}' must be of the form 'METHOD /path'"
        )


def _parse_pagination(raw: Any, context: str) -> Optional[dict]:
    if raw is None:
        return None
    if not isinstance(raw, dict):
        raise AdditionError(f"{context}: 'pagination' must be an object")
    type_ = _require_string(raw.get("type"), "pagination.type", context)
    if type_ not in ("cursor", "offset", "page"):
        raise AdditionError(
            f"{context}: pagination.type '{type_}' must be cursor, offset, or page"
        )
    cursor_param = _require_string(raw.get("cursor_param"), "pagination.cursor_param", context)
    crf = raw.get("cursor_response_field")
    cursor_response_field = None if crf is None else str(crf)
    return {"type": type_, "cursor_param": cursor_param, "cursor_response_field": cursor_response_field}


def _parse_response(raw: Any) -> Optional[dict]:
    if raw is None or not isinstance(raw, dict):
        return None
    type_ = "array" if raw.get("type") == "array" else "object"
    fields = raw.get("fields") or {}
    if not isinstance(fields, dict):
        fields = {}
    return {"type": type_, "fields": fields}


def _parse_tool(raw: dict, provider_id: str, idx: int) -> dict:
    base_ctx = f"toolkit '{provider_id}' tool [{idx}]"
    id_ = _require_string(raw.get("id"), "id", base_ctx)
    ctx = f"toolkit '{provider_id}' tool '{id_}'"
    name = _require_string(raw.get("name"), "name", ctx)
    description = _require_string(raw.get("description"), "description", ctx)
    operation = _require_string(raw.get("operation"), "operation", ctx)
    _parse_operation(operation, ctx)

    if not isinstance(raw.get("params"), list):
        raise AdditionError(f"{ctx}: 'params' must be an array (use [] for no params)")
    params = [_parse_param(p, ctx) for p in raw["params"]]

    transform = None
    if raw.get("transform") is not None:
        t = raw["transform"]
        if not isinstance(t, str) or ":" not in t:
            raise AdditionError(
                f"{ctx}: 'transform' must be of the form '{{provider}}:{{transform_name}}' or null"
            )
        transform = t

    return {
        "id": id_,
        "name": name,
        "description": description,
        "provider": provider_id,
        "operation": operation,
        "params": params,
        "transform": transform,
        "pagination": _parse_pagination(raw.get("pagination"), ctx),
        "response": _parse_response(raw.get("response")),
    }


def _parse_toolkit(raw: dict, filename: str) -> dict:
    ctx = f"toolkit {filename}"
    provider = _require_string(raw.get("provider"), "provider", ctx)
    name = _require_string(raw.get("name"), "name", ctx)
    description = _require_string(raw.get("description"), "description", ctx)
    if not isinstance(raw.get("tools"), list):
        raise AdditionError(f"{ctx}: 'tools' must be an array")
    tools = [_parse_tool(t, provider, i) for i, t in enumerate(raw["tools"])]
    return {"provider": provider, "name": name, "description": description, "tools": tools}


def load_toolkits(dir_: str, providers: dict) -> ToolkitRegistry:
    """Load all *.yaml toolkit files from *dir_*, validate, return registry keyed by provider id."""
    registry: ToolkitRegistry = {}
    for fname in sorted(os.listdir(dir_)):
        if not (fname.endswith(".yaml") or fname.endswith(".yml")):
            continue
        if fname.startswith("_"):
            continue
        with open(os.path.join(dir_, fname), encoding="utf-8") as fh:
            raw = yaml.safe_load(fh)
        cfg = _parse_toolkit(raw, fname)
        pid = cfg["provider"]
        if pid not in providers:
            raise AdditionError(
                f"toolkit {fname} references provider '{pid}' which is not defined in the provider registry"
            )
        if pid in registry:
            raise AdditionError(f"duplicate toolkit for provider '{pid}' ({fname})")
        registry[pid] = cfg
    return registry


def get_tool_definitions(registry: ToolkitRegistry, provider_id: str) -> List[dict]:
    toolkit = registry.get(provider_id)
    if toolkit is None:
        raise ToolNotFoundError(provider_id)
    return toolkit["tools"]


def get_tool_definition(registry: ToolkitRegistry, provider_id: str, tool_id: str) -> dict:
    toolkit = registry.get(provider_id)
    if toolkit is None:
        raise ToolNotFoundError(provider_id)
    tool = next((t for t in toolkit["tools"] if t["id"] == tool_id), None)
    if tool is None:
        raise ToolNotFoundError(provider_id, tool_id)
    return tool
