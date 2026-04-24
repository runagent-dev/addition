import { describe, expect, it } from 'vitest';
import { getTransform } from '../../src/transforms/index.js';

describe('transforms — registry', () => {
  it('getTransform returns a callable for a known id', () => {
    const t = getTransform('gmail:rfc2822');
    expect(typeof t).toBe('function');
  });

  it('getTransform returns null for an unknown id', () => {
    expect(getTransform('nonexistent:foo')).toBeNull();
    expect(getTransform('gmail:does_not_exist')).toBeNull();
  });

  it('getTransform is case-sensitive', () => {
    expect(getTransform('GMAIL:rfc2822')).toBeNull();
  });
});

describe('transforms — gmail:rfc2822', () => {
  const transform = getTransform('gmail:rfc2822')!;

  it('basic roundtrip: to/subject/body → base64url-decoded RFC 2822 message', () => {
    const result = transform({ to: 'alice@example.com', subject: 'Hello', body: 'Hi Alice' });
    expect(result.body).toBeDefined();
    expect(result.body.raw).toBeDefined();

    const decoded = Buffer.from(result.body.raw, 'base64url').toString('utf8');
    expect(decoded).toContain('To: alice@example.com');
    expect(decoded).toContain('Subject: Hello');
    expect(decoded).toContain('MIME-Version: 1.0');
    expect(decoded).toContain('Hi Alice');
  });

  it('headers and body are separated by a CRLF blank line (RFC 2822)', () => {
    const result = transform({
      to: 'alice@example.com',
      subject: 'Hello',
      body: 'Hi Alice',
    });
    const decoded = Buffer.from(result.body.raw, 'base64url').toString('utf8');
    // Headers section → CRLF CRLF → body
    expect(decoded).toMatch(/Subject: Hello\r\n.*\r\n\r\nHi Alice/s);
  });

  it('cc is included only when provided', () => {
    const without = transform({ to: 'a@x.com', subject: 's', body: 'b' });
    expect(Buffer.from(without.body.raw, 'base64url').toString('utf8')).not.toMatch(/^Cc:/m);

    const withCc = transform({ to: 'a@x.com', subject: 's', body: 'b', cc: 'c@x.com' });
    expect(Buffer.from(withCc.body.raw, 'base64url').toString('utf8')).toMatch(/^Cc: c@x\.com/m);
  });

  it('bcc is included only when provided', () => {
    const without = transform({ to: 'a@x.com', subject: 's', body: 'b' });
    expect(Buffer.from(without.body.raw, 'base64url').toString('utf8')).not.toMatch(/^Bcc:/m);

    const withBcc = transform({ to: 'a@x.com', subject: 's', body: 'b', bcc: 'd@x.com' });
    expect(Buffer.from(withBcc.body.raw, 'base64url').toString('utf8')).toMatch(/^Bcc: d@x\.com/m);
  });

  it('both cc and bcc when both provided', () => {
    const result = transform({
      to: 'a@x.com',
      subject: 's',
      body: 'b',
      cc: 'c@x.com',
      bcc: 'd@x.com',
    });
    const decoded = Buffer.from(result.body.raw, 'base64url').toString('utf8');
    expect(decoded).toMatch(/^Cc: c@x\.com/m);
    expect(decoded).toMatch(/^Bcc: d@x\.com/m);
  });

  it('output is URL-safe base64 (no +, /, =)', () => {
    // Use bytes that produce the troublesome base64 chars when standard-encoded.
    // `>?` encodes to "Pj8=" in standard base64; urlsafe should use no '='.
    const result = transform({ to: '>?@x.com', subject: '>>>', body: '???' });
    expect(result.body.raw).not.toContain('+');
    expect(result.body.raw).not.toContain('/');
    expect(result.body.raw).not.toContain('=');
  });

  it('handles UTF-8 in subject and body', () => {
    const result = transform({ to: 'a@x.com', subject: '日本語', body: '中文 🚀' });
    const decoded = Buffer.from(result.body.raw, 'base64url').toString('utf8');
    expect(decoded).toContain('日本語');
    expect(decoded).toContain('中文 🚀');
  });

  it('does not return params or headers (body only)', () => {
    const result = transform({ to: 'a@x.com', subject: 's', body: 'b' });
    expect(result.params).toBeUndefined();
    expect(result.headers).toBeUndefined();
  });
});

describe('transforms — google-calendar:create_event_body', () => {
  const transform = getTransform('google-calendar:create_event_body')!;

  it('maps startDateTime/endDateTime into start.dateTime/end.dateTime with timeZone', () => {
    const result = transform({
      summary: 'Meeting',
      startDateTime: '2024-06-15T09:00:00-07:00',
      endDateTime: '2024-06-15T10:00:00-07:00',
      timeZone: 'America/Los_Angeles',
    });
    expect(result.body.summary).toBe('Meeting');
    expect(result.body.start).toEqual({
      dateTime: '2024-06-15T09:00:00-07:00',
      timeZone: 'America/Los_Angeles',
    });
    expect(result.body.end).toEqual({
      dateTime: '2024-06-15T10:00:00-07:00',
      timeZone: 'America/Los_Angeles',
    });
  });

  it('defaults timeZone to UTC when omitted', () => {
    const result = transform({
      summary: 'm',
      startDateTime: '2024-06-15T09:00:00Z',
      endDateTime: '2024-06-15T10:00:00Z',
    });
    expect(result.body.start.timeZone).toBe('UTC');
    expect(result.body.end.timeZone).toBe('UTC');
  });

  it('maps attendees string[] into [{email}] objects', () => {
    const result = transform({
      summary: 'm',
      startDateTime: '2024-06-15T09:00:00Z',
      endDateTime: '2024-06-15T10:00:00Z',
      attendees: ['alice@x.com', 'bob@x.com'],
    });
    expect(result.body.attendees).toEqual([
      { email: 'alice@x.com' },
      { email: 'bob@x.com' },
    ]);
  });

  it('omits optional fields when not provided (description, location, attendees)', () => {
    const result = transform({
      summary: 'm',
      startDateTime: '2024-06-15T09:00:00Z',
      endDateTime: '2024-06-15T10:00:00Z',
    });
    expect(result.body.description).toBeUndefined();
    expect(result.body.location).toBeUndefined();
    expect(result.body.attendees).toBeUndefined();
  });

  it('includes description and location when provided', () => {
    const result = transform({
      summary: 'm',
      startDateTime: '2024-06-15T09:00:00Z',
      endDateTime: '2024-06-15T10:00:00Z',
      description: 'd',
      location: 'room 1',
    });
    expect(result.body.description).toBe('d');
    expect(result.body.location).toBe('room 1');
  });
});

describe('transforms — google-calendar:update_event_body', () => {
  const transform = getTransform('google-calendar:update_event_body')!;

  it('only includes fields that were passed (partial update)', () => {
    const result = transform({ eventId: 'evt123', summary: 'new title' });
    expect(Object.keys(result.body).sort()).toEqual(['summary']);
    expect(result.body.summary).toBe('new title');
  });

  it('wraps startDateTime/endDateTime into {dateTime} objects only when passed', () => {
    const result = transform({
      eventId: 'evt123',
      startDateTime: '2024-06-15T09:00:00Z',
    });
    expect(result.body.start).toEqual({ dateTime: '2024-06-15T09:00:00Z' });
    expect(result.body.end).toBeUndefined();
  });

  it('does not leak eventId into the body (path param)', () => {
    const result = transform({ eventId: 'evt123', summary: 'x' });
    expect(result.body.eventId).toBeUndefined();
  });

  it('empty update produces an empty body (no fields)', () => {
    const result = transform({ eventId: 'evt123' });
    expect(result.body).toEqual({});
  });

  it('includes description and location only when passed', () => {
    const withBoth = transform({
      eventId: 'evt123',
      description: 'd',
      location: 'l',
    });
    expect(withBoth.body.description).toBe('d');
    expect(withBoth.body.location).toBe('l');

    const withNone = transform({ eventId: 'evt123', summary: 's' });
    expect(withNone.body.description).toBeUndefined();
    expect(withNone.body.location).toBeUndefined();
  });

  // Issue #11 fix #6: without timeZone, Google interprets the new dateTime
  // against the event's stored tz, which silently breaks cross-tz reschedules.
  it('forwards timeZone onto each updated start/end', () => {
    const result = transform({
      eventId: 'evt1',
      startDateTime: '2024-06-15T09:00:00',
      endDateTime: '2024-06-15T10:00:00',
      timeZone: 'America/New_York',
    });
    expect(result.body.start).toEqual({
      dateTime: '2024-06-15T09:00:00',
      timeZone: 'America/New_York',
    });
    expect(result.body.end).toEqual({
      dateTime: '2024-06-15T10:00:00',
      timeZone: 'America/New_York',
    });
  });

  it('timeZone without a start/end update is a no-op (does not fabricate start/end)', () => {
    const result = transform({
      eventId: 'evt1',
      summary: 'renamed',
      timeZone: 'America/Los_Angeles',
    });
    expect(result.body.start).toBeUndefined();
    expect(result.body.end).toBeUndefined();
  });

  it('timeZone forwards onto only the side being updated', () => {
    const result = transform({
      eventId: 'evt1',
      startDateTime: '2024-06-15T09:00:00',
      timeZone: 'Europe/London',
    });
    expect(result.body.start).toEqual({
      dateTime: '2024-06-15T09:00:00',
      timeZone: 'Europe/London',
    });
    expect(result.body.end).toBeUndefined();
  });
});
