package catalog

import (
	"strings"
	"testing"
)

func TestShippedGoogleAppsDeclareTheirServiceAndProbe(t *testing.T) {
	c := shipped(t)
	cases := []struct{ app, service, probe string }{
		{"gmail", "gmail.googleapis.com", "/gmail/v1/users/me/profile"},
		{"google-drive", "drive.googleapis.com", "/drive/v3/about?fields=user(emailAddress)"},
		{"google-calendar", "calendar-json.googleapis.com", "/calendar/v3/users/me/calendarList?maxResults=1"},
		{"google-chat", "chat.googleapis.com", "/v1/spaces?pageSize=1"},
		{"google-docs", "docs.googleapis.com", ""},
	}
	for _, tc := range cases {
		p := mustProvider(t, c, tc.app)
		if p.API.Service != tc.service || p.API.ProbePath != tc.probe {
			t.Errorf("%s: service %q, probe_path %q; want %q, %q", tc.app, p.API.Service, p.API.ProbePath, tc.service, tc.probe)
		}
	}
}

func TestShippedGoogleDeclaresItsDoctorHints(t *testing.T) {
	p := mustProvider(t, shipped(t), "gmail")

	d := p.Doctor
	if d.ClientHint == "" || d.TokenAgeWarnDays != 6 || !strings.Contains(d.TokenAgeHint, "Testing") {
		t.Errorf("doctor = %+v, want a client hint, a 6-day warning and a Testing-mode hint inherited from google", d)
	}
}

func TestDoctorDataValidation(t *testing.T) {
	cases := map[string]string{
		"service with a scheme":      "  service: \"https://gmail.googleapis.com\"\n",
		"service not a string":       "  service: 7\n",
		"probe path without a slash": "  probe_path: \"gmail/v1/users/me/profile\"\n",
		"probe path with a space":    "  probe_path: \"/a b\"\n",
		"probe path not a string":    "  probe_path: [1]\n",
	}
	for name, extra := range cases {
		t.Run(name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"x.yaml": withHosts(apiBlock + extra)}), "x.yaml", "api.")
		})
	}
	badDoctor := map[string]string{
		"unknown key":     "doctor:\n  nope: 1\n",
		"zero days":       "doctor:\n  token_age_warn_days: 0\n",
		"hint not string": "doctor:\n  client_hint: [1]\n",
	}
	for name, doc := range badDoctor {
		t.Run(name, func(t *testing.T) {
			wantLoadError(t, providerFS(map[string]string{"x.yaml": baseProvider + doc}), "x.yaml", "doctor")
		})
	}
}
