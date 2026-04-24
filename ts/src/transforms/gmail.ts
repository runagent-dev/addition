import type { TransformFunction } from './index.js';

/**
 * Builds a base64url-encoded RFC 2822 MIME message from the friendly
 * `{to, subject, body, cc?, bcc?}` params Gmail's send API expects.
 *
 * NOTE — intentional divergence from `docs/spec.md §9`:
 * The spec example builds the message with `[..., '', body].filter(Boolean)`,
 * which strips the empty-string header/body separator (empty string is
 * falsy). The resulting wire format has no blank line between headers and
 * body, which Gmail rejects as malformed MIME. This implementation emits
 * an explicit `\r\n\r\n` separator per RFC 2822 §2.1. The Python port
 * must do the same — do NOT copy the spec example verbatim.
 */
const rfc2822: TransformFunction = (params) => {
  const { to, subject, body, cc, bcc } = params as {
    to: string;
    subject: string;
    body: string;
    cc?: string;
    bcc?: string;
  };

  const headers: string[] = [`To: ${to}`];
  if (cc) headers.push(`Cc: ${cc}`);
  if (bcc) headers.push(`Bcc: ${bcc}`);
  headers.push(`Subject: ${subject}`);
  headers.push('MIME-Version: 1.0');
  headers.push('Content-Type: text/plain; charset="UTF-8"');

  const message = headers.join('\r\n') + '\r\n\r\n' + body;
  const raw = Buffer.from(message, 'utf8').toString('base64url');
  return { body: { raw } };
};

export const gmailTransforms: Record<string, TransformFunction> = {
  'gmail:rfc2822': rfc2822,
};
