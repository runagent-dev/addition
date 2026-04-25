from __future__ import annotations

import re
from typing import Any, Awaitable, Callable, Dict, Optional
from urllib.parse import quote

from .errors import ToolValidationError, ToolNotFoundError
from .toolkits import get_tool_definition
from .transforms import get_transform

ProxyFn = Callable[[Dict[str, Any]], Awaitable[Dict[str, Any]]]

_METHODS_WITH_QUERY = {"GET", "DELETE"}
_PATH_PARAM_RE = re.compile(r"\{([^}]+)\}")


# ── Validation ────────────────────────────────────────────────────────────────


def _check_type(param: dict, value: Any) -> Optional[str]:
    t = param["type"]
    if t == "string":
        return None if isinstance(value, str) else "must be a string"
    if t == "integer":
        return None if isinstance(value, int) and not isinstance(value, bool) else "must be an integer"
    if t == "boolean":
        return None if isinstance(value, bool) else "must be a boolean"
    if t == "string[]":
        if not isinstance(value, list):
            return "must be an array of strings"
        return None if all(isinstance(v, str) for v in value) else "must be an array of strings"
    if t == "object":
        return None if isinstance(value, dict) else "must be an object"
    return "unknown param type"


def _validate(tool: dict, input_params: dict) -> dict:
    missing: list[str] = []
    invalid: list[dict] = []
    resolved: dict = {}

    for p in tool["params"]:
        name = p["name"]
        has_value = name in input_params and input_params[name] is not None
        if not has_value:
            if p["required"]:
                missing.append(name)
            elif p.get("default") is not None:
                resolved[name] = p["default"]
            continue

        value = input_params[name]
        type_err = _check_type(p, value)
        if type_err:
            invalid.append({"name": name, "reason": type_err})
            continue

        if p.get("enum") and value not in p["enum"]:
            invalid.append({"name": name, "reason": f"must be one of {', '.join(str(e) for e in p['enum'])}"})
            continue

        resolved[name] = value

    if missing or invalid:
        raise ToolValidationError(tool["id"], missing, invalid)
    return resolved


# ── Path interpolation ────────────────────────────────────────────────────────


def _interpolate(tool: dict, params: dict) -> tuple[str, str, dict]:
    method, template = tool["operation"].split(" ", 1)
    remaining = dict(params)
    missing_path: list[str] = []

    def replace(match: re.Match) -> str:
        name = match.group(1)
        if name not in remaining or remaining[name] is None:
            missing_path.append(name)
            return ""
        value = str(remaining.pop(name))
        return quote(value, safe="")

    path = _PATH_PARAM_RE.sub(replace, template)
    if missing_path:
        raise ToolValidationError(tool["id"], missing_path, [])
    return method, path, remaining


# ── Query stringification ─────────────────────────────────────────────────────


def _stringify_query(params: dict) -> dict[str, str]:
    out: dict[str, str] = {}
    for k, v in params.items():
        if v is None:
            continue
        if isinstance(v, list):
            if v:
                out[k] = ",".join(str(i) for i in v)
        elif isinstance(v, dict):
            import json
            out[k] = json.dumps(v)
        else:
            out[k] = str(v)
    return out


# ── Main ──────────────────────────────────────────────────────────────────────


async def execute_tool(args: dict, deps: dict) -> dict:
    """Validate params, interpolate path, apply transform, dispatch via proxy.

    args keys: user_id, provider_id, tool_id, params, connection_id (optional)
    deps keys: toolkits, proxy (async callable)
    Returns: {success, data, status}
    """
    toolkits = deps["toolkits"]
    proxy: ProxyFn = deps["proxy"]

    tool = get_tool_definition(toolkits, args["provider_id"], args["tool_id"])
    validated = _validate(tool, args["params"])
    method, path, remaining = _interpolate(tool, validated)

    body: Any = None
    query: Optional[dict] = None
    headers: dict[str, str] = {}

    if tool.get("transform"):
        transform = get_transform(tool["transform"])
        if transform is None:
            raise ToolValidationError(
                tool["id"], [],
                [{"name": "transform", "reason": f"unknown transform '{tool['transform']}'"}],
            )
        out = transform(remaining)
        if out.get("body") is not None:
            body = out["body"]
        if out.get("params"):
            query = out["params"]
        if out.get("headers"):
            headers.update(out["headers"])
    elif method in _METHODS_WITH_QUERY:
        query = _stringify_query(remaining)
    else:
        body = remaining

    req: dict[str, Any] = {
        "user_id": args["user_id"],
        "provider_id": args["provider_id"],
        "method": method,
        "path": path,
    }
    if body is not None:
        req["body"] = body
    if query is not None:
        req["params"] = query
    if headers:
        req["headers"] = headers
    if "connection_id" in args:
        req["connection_id"] = args["connection_id"]

    response = await proxy(req)
    return {
        "success": 200 <= response["status"] < 300,
        "data": response["body"],
        "status": response["status"],
    }
