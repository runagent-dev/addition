import base64

import pytest

from runagent_addition.transforms import get_transform


class TestRegistry:
    def test_returns_callable_for_known_id(self):
        t = get_transform("gmail:rfc2822")
        assert callable(t)

    def test_returns_none_for_unknown_id(self):
        assert get_transform("nonexistent:foo") is None
        assert get_transform("gmail:does_not_exist") is None

    def test_case_sensitive(self):
        assert get_transform("GMAIL:rfc2822") is None


class TestGmailRfc2822:
    def setup_method(self):
        self.transform = get_transform("gmail:rfc2822")

    def _decode(self, result: dict) -> str:
        raw = result["body"]["raw"]
        return base64.urlsafe_b64decode(raw + "==").decode("utf-8")

    def test_basic_roundtrip(self):
        result = self.transform({"to": "alice@example.com", "subject": "Hello", "body": "Hi Alice"})
        assert "body" in result
        assert "raw" in result["body"]
        decoded = self._decode(result)
        assert "To: alice@example.com" in decoded
        assert "Subject: Hello" in decoded
        assert "MIME-Version: 1.0" in decoded
        assert "Hi Alice" in decoded

    def test_headers_and_body_separated_by_crlf_blank_line(self):
        result = self.transform({"to": "alice@example.com", "subject": "Hello", "body": "Hi Alice"})
        decoded = self._decode(result)
        assert "Subject: Hello\r\n" in decoded
        assert "\r\n\r\nHi Alice" in decoded

    def test_cc_included_only_when_provided(self):
        without = self.transform({"to": "a@x.com", "subject": "s", "body": "b"})
        assert "Cc:" not in self._decode(without)

        with_cc = self.transform({"to": "a@x.com", "subject": "s", "body": "b", "cc": "c@x.com"})
        assert "Cc: c@x.com" in self._decode(with_cc)

    def test_bcc_included_only_when_provided(self):
        without = self.transform({"to": "a@x.com", "subject": "s", "body": "b"})
        assert "Bcc:" not in self._decode(without)

        with_bcc = self.transform({"to": "a@x.com", "subject": "s", "body": "b", "bcc": "d@x.com"})
        assert "Bcc: d@x.com" in self._decode(with_bcc)

    def test_both_cc_and_bcc(self):
        result = self.transform({"to": "a@x.com", "subject": "s", "body": "b", "cc": "c@x.com", "bcc": "d@x.com"})
        decoded = self._decode(result)
        assert "Cc: c@x.com" in decoded
        assert "Bcc: d@x.com" in decoded

    def test_output_is_url_safe_base64_no_padding(self):
        result = self.transform({"to": ">?@x.com", "subject": ">>>", "body": "???"})
        raw = result["body"]["raw"]
        assert "+" not in raw
        assert "/" not in raw
        assert "=" not in raw

    def test_handles_utf8_in_subject_and_body(self):
        result = self.transform({"to": "a@x.com", "subject": "日本語", "body": "中文 🚀"})
        decoded = self._decode(result)
        assert "日本語" in decoded
        assert "中文 🚀" in decoded

    def test_does_not_return_params_or_headers(self):
        result = self.transform({"to": "a@x.com", "subject": "s", "body": "b"})
        assert "params" not in result
        assert "headers" not in result


class TestGoogleCalendarCreateEventBody:
    def setup_method(self):
        self.transform = get_transform("google-calendar:create_event_body")

    def test_maps_datetime_fields_with_timezone(self):
        result = self.transform({
            "summary": "Meeting",
            "startDateTime": "2024-06-15T09:00:00-07:00",
            "endDateTime": "2024-06-15T10:00:00-07:00",
            "timeZone": "America/Los_Angeles",
        })
        assert result["body"]["summary"] == "Meeting"
        assert result["body"]["start"] == {"dateTime": "2024-06-15T09:00:00-07:00", "timeZone": "America/Los_Angeles"}
        assert result["body"]["end"] == {"dateTime": "2024-06-15T10:00:00-07:00", "timeZone": "America/Los_Angeles"}

    def test_defaults_timezone_to_utc(self):
        result = self.transform({
            "summary": "m",
            "startDateTime": "2024-06-15T09:00:00Z",
            "endDateTime": "2024-06-15T10:00:00Z",
        })
        assert result["body"]["start"]["timeZone"] == "UTC"
        assert result["body"]["end"]["timeZone"] == "UTC"

    def test_maps_attendees_to_email_objects(self):
        result = self.transform({
            "summary": "m",
            "startDateTime": "2024-06-15T09:00:00Z",
            "endDateTime": "2024-06-15T10:00:00Z",
            "attendees": ["alice@x.com", "bob@x.com"],
        })
        assert result["body"]["attendees"] == [{"email": "alice@x.com"}, {"email": "bob@x.com"}]

    def test_omits_optional_fields_when_not_provided(self):
        result = self.transform({
            "summary": "m",
            "startDateTime": "2024-06-15T09:00:00Z",
            "endDateTime": "2024-06-15T10:00:00Z",
        })
        assert "description" not in result["body"]
        assert "location" not in result["body"]
        assert "attendees" not in result["body"]

    def test_includes_description_and_location_when_provided(self):
        result = self.transform({
            "summary": "m",
            "startDateTime": "2024-06-15T09:00:00Z",
            "endDateTime": "2024-06-15T10:00:00Z",
            "description": "d",
            "location": "room 1",
        })
        assert result["body"]["description"] == "d"
        assert result["body"]["location"] == "room 1"


class TestGoogleCalendarUpdateEventBody:
    def setup_method(self):
        self.transform = get_transform("google-calendar:update_event_body")

    def test_only_includes_passed_fields(self):
        result = self.transform({"eventId": "evt123", "summary": "new title"})
        assert sorted(result["body"].keys()) == ["summary"]
        assert result["body"]["summary"] == "new title"

    def test_wraps_start_datetime_only_when_passed(self):
        result = self.transform({"eventId": "evt123", "startDateTime": "2024-06-15T09:00:00Z"})
        assert result["body"]["start"] == {"dateTime": "2024-06-15T09:00:00Z"}
        assert "end" not in result["body"]

    def test_event_id_not_leaked_into_body(self):
        result = self.transform({"eventId": "evt123", "summary": "x"})
        assert "eventId" not in result["body"]

    def test_empty_update_produces_empty_body(self):
        result = self.transform({"eventId": "evt123"})
        assert result["body"] == {}

    def test_description_and_location_only_when_passed(self):
        with_both = self.transform({"eventId": "e", "description": "d", "location": "l"})
        assert with_both["body"]["description"] == "d"
        assert with_both["body"]["location"] == "l"

        with_none = self.transform({"eventId": "e", "summary": "s"})
        assert "description" not in with_none["body"]
        assert "location" not in with_none["body"]

    def test_forwards_timezone_onto_each_updated_side(self):
        result = self.transform({
            "eventId": "evt1",
            "startDateTime": "2024-06-15T09:00:00",
            "endDateTime": "2024-06-15T10:00:00",
            "timeZone": "America/New_York",
        })
        assert result["body"]["start"] == {"dateTime": "2024-06-15T09:00:00", "timeZone": "America/New_York"}
        assert result["body"]["end"] == {"dateTime": "2024-06-15T10:00:00", "timeZone": "America/New_York"}

    def test_timezone_without_start_end_is_noop(self):
        result = self.transform({"eventId": "evt1", "summary": "renamed", "timeZone": "America/Los_Angeles"})
        assert "start" not in result["body"]
        assert "end" not in result["body"]

    def test_timezone_forwards_onto_only_side_being_updated(self):
        result = self.transform({
            "eventId": "evt1",
            "startDateTime": "2024-06-15T09:00:00",
            "timeZone": "Europe/London",
        })
        assert result["body"]["start"] == {"dateTime": "2024-06-15T09:00:00", "timeZone": "Europe/London"}
        assert "end" not in result["body"]
