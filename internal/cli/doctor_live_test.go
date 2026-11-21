package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runagent-dev/addition/internal/vault"
)

const serviceDisabledBody = `{"error":{"code":403,"message":"Gmail API has not been used in project 123456789012 before or it is disabled.",` +
	`"status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"SERVICE_DISABLED",` +
	`"domain":"googleapis.com","metadata":{"service":"gmail.googleapis.com","consumer":"projects/123456789012",` +
	`"activationUrl":"https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=123456789012",` +
	`"containerInfo":"123456789012"}}]}}`

func serviceDisabledAt(activationURL string) string {
	return strings.Replace(serviceDisabledBody, "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=123456789012", activationURL, 1)
}

func (c *callHarness) liveReport() (gotReport, result) {
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
		if len(reqs) != 1 || strings.TrimSuffix(reqs[0].Path+"?"+reqs[0].RawQuery, "?") != want {
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

			var rep gotReport
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

const numberedClientID = "123456789012-abcdefghijklmnop.apps.googleusercontent.com"

func writeClientFileWithID(t *testing.T, clientID string) string {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"installed": map[string]any{"client_id": clientID, "client_secret": fakeClientSecret}})
	path := filepath.Join(t.TempDir(), "client_secret.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDoctorLiveActivationLinkNamesTheProjectOfTheClientID(t *testing.T) {
	c := newCallHarness(t)
	c.mustRun("client", "add", "google", "--file", writeClientFileWithID(t, numberedClientID))
	c.api.respond(status(403, serviceDisabledBody, "Content-Type", "application/json"))

	rep, _ := c.liveReport()

	want := activateRoot + "gmail.googleapis.com/overview?project=123456789012"
	probe := rep.check(t, "live:gmail:work")
	if probe.Code != "api-disabled" || probe.Fix != "enable gmail.googleapis.com in the Cloud project" || probe.Hint != want {
		t.Errorf("live check = %+v, want the link %s", probe, want)
	}
	if hint := rep.check(t, "cloud-setup:gmail").Hint; !strings.Contains(hint, want) {
		t.Errorf("cloud-setup hint = %q, want it to hold %s", hint, want)
	}
}

func TestDoctorLiveActivationLinkOmitsAProjectItCannotDerive(t *testing.T) {
	for name, clientID := range map[string]string{"short number": fakeClientID, "no number": "abc-def.apps.googleusercontent.com", "no dash": "123456789012"} {
		t.Run(name, func(t *testing.T) {
			c := newCallHarness(t)
			c.mustRun("client", "add", "google", "--file", writeClientFileWithID(t, clientID))
			c.api.respond(status(403, serviceDisabledBody, "Content-Type", "application/json"))

			rep, _ := c.liveReport()

			if probe := rep.check(t, "live:gmail:work"); probe.Hint != activateRoot+"gmail.googleapis.com/overview" {
				t.Errorf("live check = %+v, want the link without a project", probe)
			}
		})
	}
}

func TestDoctorLiveHintIsTheSameWhateverTheProviderSays(t *testing.T) {
	const link = "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview"
	bodies := map[string]string{
		"percent-encoded secret":  link + "?project=%74%6f%6b%65%6e%6c%69%6b%65%2d%61%63%63%65%73%73%2d0123456789",
		"secret prefix":           link + "?project=" + lookalikeAccess[:12],
		"secret interior":         link + "?project=" + lookalikeAccess[5:20],
		"secret suffix":           link + "?project=" + lookalikeAccess[10:],
		"refresh prefix":          link + "?project=" + lookalikeRefresh[:14],
		"plausible other project": link + "?project=other-project-123",
	}
	for name, activation := range bodies {
		t.Run(name, func(t *testing.T) {
			c := newSeededLeakHarness(t)
			c.api.respond(status(403, serviceDisabledAt(activation), "Content-Type", "application/json"))

			r := c.run("doctor", "--live")

			probe := decodeDoctor(t, r).check(t, "live:gmail:work")
			if probe.Code != "api-disabled" || probe.Hint != link {
				t.Errorf("live check = %+v, want api-disabled with exactly %s", probe, link)
			}
			assertNoLeak(t, r, lookalikeAccess, lookalikeRefresh, lookalikeAccess[5:20], lookalikeAccess[:12], lookalikeRefresh[:14])
		})
	}
}

func TestDoctorLiveWithholdsTheRefreshTokenThatWasRotatedAwayDuringTheProbe(t *testing.T) {
	c := newSeededLeakHarness(t)
	c.google.configure(func(g *fakeGoogle) { g.currentRefresh, g.rotateRefresh = lookalikeRefresh, true })
	c.clock.Advance(2 * time.Hour)
	c.api.respond(func(_ int, w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+refreshedAccessPrefix) {
			t.Errorf("the probe was sent with %q, want the refreshed token", r.Header.Get("Authorization"))
		}
		status(403, serviceDisabledAt("https://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project="+lookalikeRefresh),
			"Content-Type", "application/json")(0, w, r)
	})

	r := c.run("doctor", "--live")

	assertNoLeak(t, r, lookalikeRefresh)
	if probe := decodeDoctor(t, r).check(t, "live:gmail:work"); probe.Code != "forbidden" {
		t.Errorf("live check = %+v, want the body withheld as forbidden", probe)
	}
	if n := c.google.refreshCount(); n != 1 {
		t.Errorf("refreshes = %d, want the probe to have rotated the token once", n)
	}
}

func TestDoctorLiveNeverTrustsAnActivationLinkItDoesNotRecognise(t *testing.T) {
	bare := activateRoot + "gmail.googleapis.com/overview"
	cases := map[string]string{
		"other host":          "https://evil.example/apis/api/gmail.googleapis.com/overview?project=123456789012",
		"look-alike host":     "https://console.developers.google.com.evil.example/x?project=123456789012",
		"cleartext":           "http://console.developers.google.com/apis/api/gmail.googleapis.com/overview?project=123456789012",
		"userinfo":            "https://console.developers.google.com@evil.example/?project=123456789012",
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

// seededLeakHarness has a gmail account whose tokens look like project ids, so only an echo check can tell them apart.
type seededLeakHarness struct {
	*harness
	api *fakeAPI
}

const (
	lookalikeAccess  = "tokenlike-access-0123456789"
	lookalikeRefresh = "tokenlike-refresh-9876543210"
)

func newSeededLeakHarness(t *testing.T) *seededLeakHarness {
	t.Helper()
	h := newHarness(t)
	h.addClient()
	api := newFakeAPI(t)
	h.deps.catalogFS = pointAppAPIAt(t, h.deps.catalogFS, "gmail", api.host())
	h.deps.httpClient = &http.Client{Transport: api.srv.Client().Transport, Timeout: 5 * time.Second}
	seedAccount(t, h, vault.Credential{
		AccessToken: lookalikeAccess, RefreshToken: lookalikeRefresh, TokenType: "Bearer", ExpiresAt: h.clock.Now().Add(time.Hour),
	})
	return &seededLeakHarness{harness: h, api: api}
}

// escapeAll writes every character as a \u escape, which a raw-bytes echo check cannot see.
func escapeAll(s string) string {
	var b strings.Builder
	for _, r := range s {
		fmt.Fprintf(&b, `\u%04x`, r)
	}
	return b.String()
}

func TestDoctorLiveNeverPrintsASecretFromAServiceDisabledBody(t *testing.T) {
	const link = "https://console.developers.google.com/apis/api/gmail.googleapis.com/overview"
	secrets := map[string]string{"access token": lookalikeAccess, "refresh token": lookalikeRefresh, "client secret": fakeClientSecret}
	places := map[string]func(s string) (reason, service, activation string){
		"path": func(s string) (string, string, string) {
			return "SERVICE_DISABLED", "gmail.googleapis.com", link + "/" + s + "?project=123456789012"
		},
		"query": func(s string) (string, string, string) {
			return "SERVICE_DISABLED", "gmail.googleapis.com", link + "?project=123456789012&x=" + s
		},
		"fragment": func(s string) (string, string, string) {
			return "SERVICE_DISABLED", "gmail.googleapis.com", link + "?project=123456789012#" + s
		},
		"userinfo": func(s string) (string, string, string) {
			return "SERVICE_DISABLED", "gmail.googleapis.com", "https://" + s + "@console.developers.google.com/x"
		},
		"service": func(s string) (string, string, string) { return "SERVICE_DISABLED", s, link + "?project=123456789012" },
		"reason": func(s string) (string, string, string) {
			return "SERVICE_DISABLED-" + s, "gmail.googleapis.com", link + "?project=123456789012"
		},
		"project": func(s string) (string, string, string) {
			return "SERVICE_DISABLED", "gmail.googleapis.com", link + "?project=" + s
		},
	}
	for secretName, secret := range secrets {
		for placeName, place := range places {
			for encoding, encode := range map[string]func(string) string{"raw": func(s string) string { return s }, "escaped": escapeAll} {
				t.Run(secretName+"/"+placeName+"/"+encoding, func(t *testing.T) {
					c := newSeededLeakHarness(t)
					reason, service, activation := place(encode(secret))
					body := `{"error":{"code":403,"details":[{"reason":"` + reason + `","metadata":{"service":"` + service + `","activationUrl":"` + activation + `"}}]}}`
					c.api.respond(status(403, body, "Content-Type", "application/json"))

					r := c.run("doctor", "--live")

					assertNoLeak(t, r, secret)
					rep := decodeDoctor(t, r)
					if probe := rep.check(t, "live:gmail:work"); probe.Code != "forbidden" {
						t.Errorf("live check = %+v, want the body withheld as forbidden", probe)
					}
				})
			}
		}
	}
}

func TestDoctorLiveIgnoresAServiceNameTheCatalogDoesNotHave(t *testing.T) {
	c := newCallHarness(t)
	body := strings.Replace(serviceDisabledBody, `"service":"gmail.googleapis.com"`, `"service":"other.googleapis.com"`, 1)
	c.api.respond(status(403, body, "Content-Type", "application/json"))

	rep, _ := c.liveReport()

	want := activateRoot + "gmail.googleapis.com/overview"
	if probe := rep.check(t, "live:gmail:work"); probe.Code != "api-disabled" || probe.Hint != want || probe.Fix != "enable gmail.googleapis.com in the Cloud project" {
		t.Errorf("live check = %+v, want the catalog's service and link %s", probe, want)
	}
}

func TestDoctorLiveStalledErrorBodyIsUnavailable(t *testing.T) {
	probeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { probeTimeout = defaultProbeTimeout })
	c := newCallHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c.api.respond(func(_ int, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"details":[`))
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	finished := make(chan result, 1)
	go func() { finished <- c.run("doctor", "--live") }()

	select {
	case r := <-finished:
		rep := wantHealthy(t, r)
		if probe := rep.check(t, "live:gmail:work"); probe.Status != "warn" || probe.Code != "unavailable" {
			t.Errorf("live check = %+v, want unavailable", probe)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("doctor --live hung on a stalled error body")
	}
}

func TestDoctorLiveStalledRefreshEndsWithTheTokenEndpointsOwnTimeout(t *testing.T) {
	probeTimeout = 50 * time.Millisecond
	t.Cleanup(func() { probeTimeout = defaultProbeTimeout })
	c := newCallHarness(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	c.google.configure(func(g *fakeGoogle) { g.onToken = func() { <-release } })
	c.deps.httpClient = &http.Client{Transport: c.transport, Timeout: 300 * time.Millisecond}
	c.expireToken()
	finished := make(chan result, 1)
	go func() { finished <- c.run("doctor", "--live") }()

	select {
	case r := <-finished:
		rep := wantHealthy(t, r)
		if probe := rep.check(t, "live:gmail:work"); probe.Status != "warn" || probe.Code != "unavailable" {
			t.Errorf("live check = %+v, want unavailable", probe)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("doctor --live outlived the token endpoint's timeout")
	}
}
