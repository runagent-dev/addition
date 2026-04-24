import type { TransformFunction } from './index.js';

/**
 * Builds a base64url-encoded RFC 2822 MIME message from the friendly
 * `{to, subject, body, cc?, bcc?}` params Gmail's send API expects.
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

  // Headers → CRLF CRLF → body (RFC 2822 §2.1)
  const message = headers.join('\r\n') + '\r\n\r\n' + body;
  const raw = Buffer.from(message, 'utf8').toString('base64url');
  return { body: { raw } };
};

export const gmailTransforms: Record<string, TransformFunction> = {
  'gmail:rfc2822': rfc2822,
};
