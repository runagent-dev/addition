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
