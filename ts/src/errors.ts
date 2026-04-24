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

export class ProviderDisabledError extends AdditionError {
  constructor(public readonly providerId: string) {
    super(`Provider '${providerId}' is disabled`);
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

export class OAuthError extends AdditionError {
  constructor(
    public readonly code: string,
    public readonly description?: string,
  ) {
    super(description ? `OAuth error '${code}': ${description}` : `OAuth error '${code}'`);
  }
}

export class SchemaError extends AdditionError {}

export class ConnectionNotFoundError extends AdditionError {
  constructor(public readonly connectionId: string) {
    super(`Connection '${connectionId}' not found`);
  }
}

export class ConnectionExpiredError extends AdditionError {
  constructor(public readonly connectionId: string) {
    super(`Connection '${connectionId}' returned 401 and has no refresh token`);
  }
}

export class ProxyError extends AdditionError {
  constructor(
    public readonly status: number,
    public readonly code: string,
    public readonly description?: string,
  ) {
    super(description ? `Proxy error ${status} '${code}': ${description}` : `Proxy error ${status} '${code}'`);
  }
}

export class TokenRefreshError extends AdditionError {
  constructor(
    public readonly code: string,
    public readonly description?: string,
  ) {
    super(description ? `Token refresh failed '${code}': ${description}` : `Token refresh failed '${code}'`);
  }
}

export class ToolValidationError extends AdditionError {
  constructor(
    public readonly toolId: string,
    public readonly missing: string[],
    public readonly invalid: Array<{ name: string; reason: string }>,
  ) {
    const parts: string[] = [];
    if (missing.length) parts.push(`missing required: ${missing.join(', ')}`);
    if (invalid.length) {
      parts.push(`invalid: ${invalid.map((i) => `${i.name} (${i.reason})`).join('; ')}`);
    }
    super(`tool '${toolId}': ${parts.join('; ') || 'validation failed'}`);
  }
}
