class AdditionError(Exception):
    pass


class EncryptionError(AdditionError):
    pass


class ProviderNotFoundError(AdditionError):
    def __init__(self, provider_id: str) -> None:
        super().__init__(f"provider '{provider_id}' not found")
        self.provider_id = provider_id
