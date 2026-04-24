export class AdditionError extends Error {
  constructor(message: string, public readonly cause?: unknown) {
    super(message);
    this.name = new.target.name;
  }
}

export class EncryptionError extends AdditionError {}

export class ProviderNotFoundError extends AdditionError {
  constructor(public readonly providerId: string) {
    super(`Provider '${providerId}' is not defined`);
  }
}
