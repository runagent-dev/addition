from __future__ import annotations


class AdditionError(Exception):
    pass


class EncryptionError(AdditionError):
    pass


class ProviderNotFoundError(AdditionError):
    def __init__(self, provider_id: str) -> None:
        super().__init__(f"provider '{provider_id}' not found")
        self.provider_id = provider_id


class ToolNotFoundError(AdditionError):
    def __init__(self, provider_id: str, tool_id: str | None = None) -> None:
        if tool_id:
            super().__init__(f"tool '{tool_id}' not found in toolkit '{provider_id}'")
        else:
            super().__init__(f"toolkit for provider '{provider_id}' not found")
        self.provider_id = provider_id
        self.tool_id = tool_id


class SchemaError(AdditionError):
    pass


class ToolValidationError(AdditionError):
    def __init__(
        self,
        tool_id: str,
        missing: list[str],
        invalid: list[dict[str, str]],
    ) -> None:
        parts: list[str] = []
        if missing:
            parts.append(f"missing required: {', '.join(missing)}")
        if invalid:
            parts.append("invalid: " + "; ".join(f"{i['name']} ({i['reason']})" for i in invalid))
        super().__init__(f"tool '{tool_id}': {'; '.join(parts) or 'validation failed'}")
        self.tool_id = tool_id
        self.missing = missing
        self.invalid = invalid
