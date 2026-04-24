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

export class ToolNotFoundError extends AdditionError {
  constructor(public readonly providerId: string, public readonly toolId?: string) {
    super(
      toolId
        ? `Tool '${toolId}' not found in toolkit '${providerId}'`
        : `Toolkit for provider '${providerId}' is not defined`,
    );
  }
}
