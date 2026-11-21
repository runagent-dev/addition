package cli

import (
	"slices"
	"testing"
)

// sharedHostHarness has Drive and Calendar connected as work, both served by one fake host,
// as www.googleapis.com serves both, with each app's token distinct.
func sharedHostHarness(t *testing.T) (*callHarness, *fakeAPI) {
	t.Helper()
	c := newCallHarness(t, distinctTokensPerApp)
	shared := newFakeAPI(t)
	c.attachRoutedApp("google-drive", "work", workSubject, shared)
	c.attachRoutedApp("google-calendar", "work", workSubject, shared)
	return c, shared
}

func TestAnAppTokenNeverReachesAnotherAppsRoutesOnASharedHost(t *testing.T) {
	c, shared := sharedHostHarness(t)

	c.wantError(c.run("call", "drive", "-a", "work", "GET", "/calendar/v3/users/me/calendarList"), ExitUsage, "path-not-allowed")
	c.wantError(c.run("call", "calendar", "-a", "work", "GET", "/drive/v3/files"), ExitUsage, "path-not-allowed")
	c.wantError(c.run("call", "drive", "-a", "work", "GET", shared.url("/calendar/v3/users/me/calendarList")), ExitUsage, "path-not-allowed")
	c.wantError(c.run("call", "calendar", "-a", "work", "GET", shared.url("/drive/v3/files")), ExitUsage, "path-not-allowed")

	if n := len(shared.requests()); n != 0 {
		t.Errorf("the shared host received %d requests, want none", n)
	}
	if c.transport.total() != 0 {
		t.Errorf("%d requests left addn; a refused target must not even refresh a token", c.transport.total())
	}
}

func TestAnAppsOwnRoutesStillWorkOnASharedHost(t *testing.T) {
	c, shared := sharedHostHarness(t)

	c.mustRun("call", "drive", "-a", "work", "GET", "/drive/v3/files")
	c.mustRun("call", "drive", "-a", "work", "GET", shared.url("/upload/drive/v3/files"))
	c.mustRun("call", "calendar", "-a", "work", "GET", "/calendar/v3/calendars/primary/events")

	var auths []string
	for _, r := range shared.requests() {
		auths = append(auths, r.Method+" "+r.Path+" "+r.Header.Get("Authorization"))
	}
	want := []string{
		"GET /drive/v3/files Bearer " + fakeAccessToken + "-drive",
		"GET /upload/drive/v3/files Bearer " + fakeAccessToken + "-drive",
		"GET /calendar/v3/calendars/primary/events Bearer " + fakeAccessToken + "-calendar",
	}
	if !slices.Equal(auths, want) {
		t.Errorf("requests = %v, want %v", auths, want)
	}
}

func TestRouteTricksNeverReachTheSharedHost(t *testing.T) {
	c, shared := sharedHostHarness(t)

	for _, path := range []string{
		"/drive/../calendar/v3/users/me/calendarList",
		"/drive/%2e%2e/calendar/v3/users/me/calendarList",
		"/drive/..%2fcalendar/v3/users/me/calendarList",
		"/drive/..\\calendar/v3/users/me/calendarList",
		"//calendar/v3/users/me/calendarList",
		"/drive//calendar/v3/users/me/calendarList",
		"/drive/%09/calendar",
		shared.url("/drive/../calendar/v3/users/me/calendarList"),
		shared.url("/drive/%2E%2E/calendar/v3/users/me/calendarList"),
	} {
		r := c.run("call", "drive", "-a", "work", "GET", path)

		if r.code != ExitUsage {
			t.Errorf("%q: exit = %d, want %d\nstderr: %s", path, r.code, ExitUsage, r.stderr)
		}
	}
	if n := len(shared.requests()); n != 0 || c.transport.total() != 0 {
		t.Errorf("%d requests reached the host and %d left addn; want none", n, c.transport.total())
	}
}
