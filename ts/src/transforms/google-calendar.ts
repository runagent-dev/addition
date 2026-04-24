import type { TransformFunction } from './index.js';

const createEventBody: TransformFunction = (params) => {
  const {
    summary,
    description,
    startDateTime,
    endDateTime,
    timeZone,
    location,
    attendees,
  } = params as {
    summary: string;
    description?: string;
    startDateTime: string;
    endDateTime: string;
    timeZone?: string;
    location?: string;
    attendees?: string[];
  };

  const tz = timeZone || 'UTC';
  const body: Record<string, unknown> = {
    summary,
    start: { dateTime: startDateTime, timeZone: tz },
    end: { dateTime: endDateTime, timeZone: tz },
  };
  if (description !== undefined) body.description = description;
  if (location !== undefined) body.location = location;
  if (attendees !== undefined) body.attendees = attendees.map((email) => ({ email }));
  return { body };
};

/**
 * Partial-update mapper. Only emits fields the caller actually passed, so
 * unspecified fields don't clobber whatever the event currently has.
 * `eventId` is a path param and must not appear in the body.
 */
const updateEventBody: TransformFunction = (params) => {
  const p = params as Record<string, unknown>;
  const body: Record<string, unknown> = {};
  if (p.summary !== undefined) body.summary = p.summary;
  if (p.description !== undefined) body.description = p.description;
  if (p.location !== undefined) body.location = p.location;
  if (p.startDateTime !== undefined) body.start = { dateTime: p.startDateTime };
  if (p.endDateTime !== undefined) body.end = { dateTime: p.endDateTime };
  return { body };
};

export const googleCalendarTransforms: Record<string, TransformFunction> = {
  'google-calendar:create_event_body': createEventBody,
  'google-calendar:update_event_body': updateEventBody,
};
