from __future__ import annotations

import os
from typing import Any, Dict

import yaml

from .errors import AdditionError, ProviderNotFoundError

ProviderConfig = Dict[str, Any]
ProviderRegistry = Dict[str, ProviderConfig]


def _read_raw_providers(dir_: str) -> list[dict]:
    raws = []
    for fname in os.listdir(dir_):
        if not (fname.endswith(".yaml") or fname.endswith(".yml")):
            continue
        if fname.startswith("_"):
            continue
        path = os.path.join(dir_, fname)
        with open(path, encoding="utf-8") as fh:
            raw = yaml.safe_load(fh)
        if not isinstance(raw, dict) or not isinstance(raw.get("id"), str) or raw["id"] == "":
            raise AdditionError(f"provider file {fname} is missing a non-empty 'id'")
        raws.append(raw)
    return raws


def _topo_sort(raws: list[dict]) -> list[dict]:
    by_id: dict[str, dict] = {r["id"]: r for r in raws}
    sorted_nodes: list[dict] = []
    state: dict[str, str] = {}

    def visit(id_: str, stack: list[str]) -> None:
        mark = state.get(id_)
        if mark == "done":
            return
        if mark == "visiting":
            raise AdditionError(f"extends cycle detected: {' -> '.join([*stack, id_])}")
        node = by_id.get(id_)
        if node is None:
            raise AdditionError(f"provider '{id_}' extends unknown provider (missing YAML)")
        state[id_] = "visiting"
        parent = node.get("extends")
        if parent:
            if parent not in by_id:
                raise AdditionError(f"provider '{id_}' extends unknown provider '{parent}'")
            visit(parent, [*stack, id_])
        state[id_] = "done"
        sorted_nodes.append(node)

    for r in raws:
        visit(r["id"], [])
    return sorted_nodes


def _deep_merge(base: dict, child: dict) -> dict:
    """Merge child onto base. Scalars: child wins. Objects: recurse. Arrays: child replaces."""
    out = dict(base)
    for key, child_val in child.items():
        if key == "extends":
            continue
        base_val = base.get(key)
        if (
            isinstance(base_val, dict)
            and isinstance(child_val, dict)
        ):
            out[key] = _deep_merge(base_val, child_val)
        else:
            out[key] = child_val
    return out


def _merge_configs(base: dict, child: dict) -> dict:
    child_name = child.get("name")
    if not isinstance(child_name, str) or child_name == "":
        raise AdditionError(
            f"provider '{child['id']}' uses 'extends: {child.get('extends')}' but does not "
            f"declare its own 'name'; child configs must provide their own name"
        )
    merged = _deep_merge(base, child)
    merged["id"] = child["id"]
    merged["name"] = child["name"]
    return merged


def _require_non_empty_string(provider_id: str, path: str, value: Any) -> None:
    if not isinstance(value, str) or value == "":
        raise AdditionError(
            f"provider '{provider_id}': missing required string field '{path}' after extends resolution"
        )


def _require_bool(provider_id: str, path: str, value: Any) -> None:
    if not isinstance(value, bool):
        raise AdditionError(
            f"provider '{provider_id}': field '{path}' must be a boolean, got {type(value).__name__}"
        )


def _require_string_list(provider_id: str, path: str, value: Any) -> None:
    if not isinstance(value, list) or any(not isinstance(v, str) for v in value):
        raise AdditionError(f"provider '{provider_id}': field '{path}' must be a string[]")


def _require_dict(provider_id: str, path: str, value: Any) -> None:
    if not isinstance(value, dict):
        raise AdditionError(f"provider '{provider_id}': field '{path}' must be a plain object")


def _require_one_of(provider_id: str, path: str, value: Any, allowed: tuple) -> None:
    if value not in allowed:
        raise AdditionError(
            f"provider '{provider_id}': field '{path}' must be one of "
            f"{', '.join(allowed)}, got {value!r}"
        )


def _validate_resolved(cfg: dict) -> None:
    id_ = cfg["id"]
    _require_non_empty_string(id_, "name", cfg.get("name"))
    _require_one_of(id_, "auth_type", cfg.get("auth_type"), ("oauth2", "api_key", "basic"))

    if cfg.get("auth_type") == "oauth2":
        o = cfg.get("oauth2")
        if not isinstance(o, dict):
            raise AdditionError(f"provider '{id_}': auth_type=oauth2 but no 'oauth2' block")
        _require_non_empty_string(id_, "oauth2.authorize_url", o.get("authorize_url"))
        _require_non_empty_string(id_, "oauth2.token_url", o.get("token_url"))
        revoke = o.get("revoke_url")
        if revoke is not None and (not isinstance(revoke, str) or revoke == ""):
            raise AdditionError(
                f"provider '{id_}': field 'oauth2.revoke_url' must be a non-empty string, null, or absent"
            )
        _require_bool(id_, "oauth2.refresh_supported", o.get("refresh_supported"))
        _require_bool(id_, "oauth2.pkce_supported", o.get("pkce_supported"))
        _require_bool(id_, "oauth2.pkce_required", o.get("pkce_required"))
        if o.get("pkce_required") and not o.get("pkce_supported"):
            raise AdditionError(
                f"provider '{id_}': 'oauth2.pkce_required' is true but 'oauth2.pkce_supported' is false — contradiction"
            )
        _require_string_list(id_, "oauth2.default_scopes", o.get("default_scopes"))
        _require_one_of(id_, "oauth2.scopes_delimiter", o.get("scopes_delimiter"), ("space", "comma"))
        _require_dict(id_, "oauth2.authorize_params", o.get("authorize_params"))
        _require_dict(id_, "oauth2.token_params", o.get("token_params"))
        _require_one_of(id_, "oauth2.token_response_format", o.get("token_response_format"), ("json", "form"))
        _require_one_of(id_, "oauth2.client_auth_method", o.get("client_auth_method"), ("body", "header"))
        mapping = o.get("token_response_mapping")
        if mapping is not None:
            _require_dict(id_, "oauth2.token_response_mapping", mapping)
            _require_non_empty_string(id_, "oauth2.token_response_mapping.access_token", mapping.get("access_token"))
            _require_non_empty_string(id_, "oauth2.token_response_mapping.refresh_token", mapping.get("refresh_token"))
            _require_non_empty_string(id_, "oauth2.token_response_mapping.expires_in", mapping.get("expires_in"))

    api = cfg.get("api")
    if not isinstance(api, dict):
        raise AdditionError(f"provider '{id_}': no 'api' block")
    _require_non_empty_string(id_, "api.base_url", api.get("base_url"))
    _require_non_empty_string(id_, "api.auth_header", api.get("auth_header"))
    _require_non_empty_string(id_, "api.auth_prefix", api.get("auth_prefix"))


def load_providers(dir_: str) -> ProviderRegistry:
    """Load all *.yaml provider files from *dir_*, resolve extends chains, return registry."""
    raws = _read_raw_providers(dir_)
    sorted_nodes = _topo_sort(raws)
    resolved: dict[str, dict] = {}

    for node in sorted_nodes:
        merged = dict(node)
        parent_id = node.get("extends")
        if parent_id:
            base = resolved.get(parent_id)
            if base is None:
                raise AdditionError(
                    f"provider '{node['id']}' extends '{parent_id}' which was not resolved"
                )
            merged = _merge_configs(base, node)
        merged.pop("extends", None)
        _validate_resolved(merged)
        resolved[node["id"]] = merged

    return resolved


def get_provider(registry: ProviderRegistry, provider_id: str) -> ProviderConfig:
    cfg = registry.get(provider_id)
    if cfg is None:
        raise ProviderNotFoundError(provider_id)
    return cfg
