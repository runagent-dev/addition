package cli

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

const serviceDisabledBody = `{"error":{"code":403,"message":"Gmail API has not been used in project 123456789012 before or it is disabled.",` +
	`"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED",` +
	`"domain":"googleapis.com","metadata":{"service":"gmail.googleapis.com","consumer":"projects/123456789012",` +
	`"activationUrl":"https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=123456789012",` +
	`"containerInfo":"123456789012"}}]}}`

func serviceDisabledAt(activationURL string) string {
	return strings.Replace(serviceDisabledBody, "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=123456789012", activationURL, 1)
}

func (c *callHarness) liveReport() (doctorReport, result) {
	c.t.Helper()
	r := c.run("doctor", "--live")
	return decodeDoctor(c.t, r), r
}

func TestDoctorLiveHealthyProbe(t *testing.T) {
	c := newCallHarness(t)

	rep := wantHealthy(t, c.run("doctor", "--live"))

	probe := rep.check(t, "live:gmail:work")
	if probe.Status != "ok" || probe.Code != "ok" {
		t.Errorf("live check = %+v", probe)
	}
	reqs := c.api.requests()
	if len(reqs) != 1 || reqs[0].Method != "GET" || reqs[0].Path != "/gmail/v1/users/me/profile" || reqs[0].Header.Get("Authorization") != "Bearer "+fakeAccessToken {
		t.Errorf("requests = %+v, want one authenticated GET of the profile", reqs)
	}
	if events := auditEvents(t, c.home); events[len(events)-1] != "call" {
		t.Errorf("audit events = %v, want the probe recorded as a call", events)
	}
}

func TestDoctorLiveUsesEachAppsProbePath(t *testing.T) {
	c := newCallHarness(t)
	drive := c.attachApp("google-drive", "work", workSubject)
	calendar := c.attachApp("google-calendar", "work", workSubject)
	chat := c.attachApp("google-chat", "work", workSubject)
	docs := c.attachApp("google-docs", "work", workSubject)

	rep := wantHealthy(t, c.run("doctor", "--live"))

	for api, want := range map[*fakeAPI]string{
		c.api:    "/gmail/v1/users/me/profile",
		drive:    "/drive/v3/about?fields=user(emailAddress)",
		calendar: "/calendar/v3/users/me/calendarList?maxResults=1",
		chat:     "/v1/spaces?pageSize=1",
	} {
		reqs := api.requests()
		if len(reqs) != 1 || reqs[0].Path+"?"+reqs[0].RawQuery != want {
			t.Errorf("requests = %+v, want one GET of %s", reqs, want)
		}
	}
	if len(docs.requests()) != 0 {
		t.Error("google-docs has no probe path but was probed")
	}
	if c := rep.check(t, "live:google-docs:work"); c.Status != "skip" {
		t.Errorf("docs live check = %+v, want skip", c)
	}
}

func TestDoctorLiveClassifications(t *testing.T) {
	cases := []struct {
		name    string
		respond func(n int, w http.ResponseWriter, r *http.Request)
		status  string
		code    string
		next    string
	}{
		{"unauthorized", status(401, `{"error":{"code":401,"message":"`+echoMarker+`"}}`, "Content-Type", "application/json"),
			"fail", "needs-reauth", "addition connect gmail --as work"},
		{"service disabled", status(403, serviceDisabledBody, "Content-Type", "application/json"),
			"fail", "api-disabled", "enable gmail.googleapis.com in the Cloud project"},
		{"other forbidden", status(403, `{"error":{"code":403,"message":"`+echoMarker+`","details":[{"reason":"RATE_LIMIT_EXCEEDED"}]}}`, "Content-Type", "application/json"),
			"fail", "forbidden", "addition connect gmail --as work"},
		{"forbidden without json", status(403, "<html>"+echoMarker+"</html>", "Content-Type", "text/html"),
			"fail", "forbidden", "addition connect gmail --as work"},
		{"throttled", status(429, "slow down "+echoMarker), "warn", "unavailable", ""},
		{"server error", status(503, echoMarker), "warn", "unavailable", ""},
		{"unexpected status", status(404, echoMarker), "warn", "unavailable", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newCallHarness(t)
			c.api.respond(tc.respond)

			r := c.run("doctor", "--live")

			var rep doctorReport
			if tc.next == "" {
				rep = wantHealthy(t, r)
			} else {
				rep = wantNeedsAction(t, r, tc.next)
			}
			probe := rep.check(t, "live:gmail:work")
			if probe.Status != tc.status || probe.Code != tc.code {
				t.Errorf("live check = %+v, want %s / %s", probe, tc.status, tc.code)
			}
			if strings.Contains(r.stdout+r.stderr, echoMarker) {
				t.Error("provider text reached the output")
			}
		})
	}
}

func TestDoctorLiveServiceDisabledBuildsTheActivationLink(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(403, serviceDisabledBody, "Content-Type", "application/json"))

	rep, _ := c.liveReport()

	probe := rep.check(t, "live:gmail:work")
	want := activateRoot + "gmail.googleapis.com/overview?project=123456789012"
	if probe.Fix != "enable gmail.googleapis.com in the Cloud project" || probe.Hint != want {
		t.Errorf("live check = %+v, want fix to enable the service and hint %s", probe, want)
	}
}

func TestDoctorLiveNeverTrustsAnActivationLinkItDoesNotRecognise(t *testing.T) {
	bare := activateRoot + "gmail.googleapis.com/overview"
	cases := map[string]string{
		"other host":          "https://evil.example/apis/api/gmail.googleapis.com/overview?project=123456789012",
		"look-alike host":     "https://console.developers.google.com.evil.example/x?project=123456789012",
		"cleartext":           "http://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=123456789012",
		"userinfo":            "https://console.developers.google.com@evil.example/?project=123456789012",
		"project is a token":  "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=" + fakeAccessToken,
		"project with markup": "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=a%26b%3Dc",
		"no project":          "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview",
	}
	for name, activation := range cases {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			c.api.respond(status(403, serviceDisabledAt(activation), "Content-Type", "application/json"))

			rep, r := c.liveReport()

			probe := rep.check(t, "live:gmail:work")
			if probe.Code != "api-disabled" || probe.Hint != bare {
				t.Errorf("live check = %+v, want api-disabled with the bare link %s", probe, bare)
			}
			assertNoLeak(t, r, "evil.example", fakeAccessToken)
		})
	}
}

func TestDoctorLiveWithholdsABodyThatEchoesTheAccessToken(t *testing.T) {
	for name, status := range map[string]int{"forbidden": 403, "server error": 500, "unauthorized": 401, "ok": 200} {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			hostile := `{"error":{"message":"` + fakeAccessToken + " " + fakeClientSecret + " " + fakeRefreshToken + `"}}`
			c.api.respond(func(n int, w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(hostile))
			})

			rep, r := c.liveReport()

			assertNoLeak(t, r, fakeAccessToken, fakeClientSecret, fakeRefreshToken)
			if probe := rep.check(t, "live:gmail:work"); probe.Summary == "" {
				t.Errorf("live check = %+v", probe)
			}
		})
	}
}

func TestDoctorLiveWithholdsAContentCodedErrorBody(t *testing.T) {
	c := newCallHarness(t)
	c.api.respond(status(403, serviceDisabledBody, "Content-Type", "application/json", "Content-Encoding", "gzip"))

	rep, _ := c.liveReport()

	if probe := rep.check(t, "live:gmail:work"); probe.Code != "forbidden" {
		t.Errorf("live check = %+v, want forbidden: a coded body cannot be inspected", probe)
	}
}

func TestDoctorLiveNetworkFailureIsNotAnAccountFailure(t *testing.T) {
	c := newCallHarness(t)
	c.api.srv.Close()

	rep := wantHealthy(t, c.run("doctor", "--live"))

	if probe := rep.check(t, "live:gmail:work"); probe.Status != "warn" || probe.Code != "unavailable" {
		t.Errorf("live check = %+v", probe)
	}
}

func TestDoctorLiveRevokedRefreshTokenNeedsReauth(t *testing.T) {
	c := newCallHarness(t, func(g *fakeGoogle) { g.refreshError = "invalid_grant" })
	c.expireToken()

	rep := wantNeedsAction(t, c.run("doctor", "--live"), "addition connect gmail --as work")

	if probe := rep.check(t, "live:gmail:work"); probe.Status != "fail" || probe.Code != "needs-reauth" {
		t.Errorf("live check = %+v", probe)
	}
	if n := len(c.api.requests()); n != 0 {
		t.Errorf("%d API requests after a rejected refresh, want none", n)
	}
}

func TestDoctorLiveDoesNotProbeAccountsThatAlreadyNeedAction(t *testing.T) {
	h := newHarness(t)
	h.addClient()
	seedLegacyAccountWithExpiredCredential(t, h)
	h.deps.httpClient = &http.Client{Transport: failingTransport{t}}

	rep := wantNeedsAction(t, h.run("doctor", "--live"), "addition accounts remove google:work")

	if probe := rep.check(t, "live:google:work"); probe.Status != "skip" {
		t.Errorf("live check = %+v, want skip", probe)
	}
}

func TestDoctorLiveSendsTheTokenOnlyWhereTheAppAllows(t *testing.T) {
	c := newCallHarness(t)

	c.run("doctor", "--live")

	if hosts := c.transport.seen; !slices.Equal(hosts, []string{c.api.host()}) {
		t.Errorf("requests went to %v, want only the app's API host", hosts)
	}
}
