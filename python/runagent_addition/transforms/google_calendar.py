from __future__ import annotations

from typing import Any, Dict


def _create_event_body(params: Dict[str, Any]) -> Dict[str, Any]:
    tz: str = params.get("timeZone") or "UTC"
    body: Dict[str, Any] = {
        "summary": params["summary"],
        "start": {"dateTime": params["startDateTime"], "timeZone": tz},
        "end": {"dateTime": params["endDateTime"], "timeZone": tz},
    }
    if "description" in params:
        body["description"] = params["description"]
    if "location" in params:
        body["location"] = params["location"]
    if "attendees" in params:
        body["attendees"] = [{"email": e} for e in params["attendees"]]
    return {"body": body}


def _update_event_body(params: Dict[str, Any]) -> Dict[str, Any]:
    """Partial-update mapper — only emits fields the caller passed.

    `eventId` is a path param and must not appear in the body.
    `timeZone` is attached to each start/end being updated so Google Calendar
    doesn't silently interpret dateTime against the event's stored tz.
    """
    body: Dict[str, Any] = {}
    if "summary" in params:
        body["summary"] = params["summary"]
    if "description" in params:
        body["description"] = params["description"]
    if "location" in params:
        body["location"] = params["location"]

    tz = params.get("timeZone") or None

    if "startDateTime" in params:
        start: Dict[str, Any] = {"dateTime": params["startDateTime"]}
        if tz:
            start["timeZone"] = tz
        body["start"] = start

    if "endDateTime" in params:
        end: Dict[str, Any] = {"dateTime": params["endDateTime"]}
        if tz:
            end["timeZone"] = tz
        body["end"] = end

    return {"body": body}


google_calendar_transforms = {
    "google-calendar:create_event_body": _create_event_body,
    "google-calendar:update_event_body": _update_event_body,
}
