export { Addition } from './addition.js';
export type { AdditionOptions } from './addition.js';

export {
  AdditionError,
  EncryptionError,
  SchemaError,
  ProviderNotFoundError,
  ProviderDisabledError,
  ToolNotFoundError,
  ToolValidationError,
  OAuthError,
  ConnectionNotFoundError,
  ConnectionExpiredError,
  ProxyError,
  TokenRefreshError,
} from './errors.js';

export type { ProviderConfig, ToolDefinition } from './types.js';
export type { CallbackResult } from './oauth.js';
export type { ConnectionRecord } from './tokens.js';
export type { ProxyArgs, ProxyResponse } from './proxy.js';
export type { ExecuteToolArgs, ToolExecutionResult } from './tools.js';
