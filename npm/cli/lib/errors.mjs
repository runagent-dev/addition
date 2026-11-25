/** A failure with a stable code, printed as the same JSON envelope the addition binary uses. */
export class ShimError extends Error {
  constructor(code, message, options) {
    super(message, options);
    this.code = code;
  }
}

export function envelope(error) {
  const code = error instanceof ShimError ? error.code : "internal";
  return JSON.stringify({ error: { code, message: error.message } });
}
