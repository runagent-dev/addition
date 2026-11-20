package vault_test

import (
	"path/filepath"
	"testing"

	"github.com/runagent-dev/addition/internal/vault"
)

func TestResolveHome(t *testing.T) {
	cases := map[string]struct {
		goos string
		vars map[string]string
		want string
	}{
		"ADDN_HOME wins": {
			"linux", map[string]string{"ADDN_HOME": "/srv/addn", "XDG_DATA_HOME": "/xdg", "HOME": "/home/u"}, "/srv/addn",
		},
		"ADDN_HOME wins on windows": {
			"windows", map[string]string{"ADDN_HOME": "/srv/addn", "LOCALAPPDATA": "/appdata"}, "/srv/addn",
		},
		"linux XDG_DATA_HOME": {
			"linux", map[string]string{"XDG_DATA_HOME": "/xdg", "HOME": "/home/u"}, "/xdg/addition",
		},
		"linux relative XDG_DATA_HOME is ignored": {
			"linux", map[string]string{"XDG_DATA_HOME": "rel/xdg", "HOME": "/home/u"}, "/home/u/.local/share/addition",
		},
		"linux default": {
			"linux", map[string]string{"HOME": "/home/u"}, "/home/u/.local/share/addition",
		},
		"macOS": {
			"darwin", map[string]string{"HOME": "/Users/u", "XDG_DATA_HOME": "/xdg"}, "/Users/u/Library/Application Support/addition",
		},
		"windows": {
			"windows", map[string]string{"LOCALAPPDATA": "/appdata"}, filepath.Join("/appdata", "addition"),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := vault.Env{GOOS: tc.goos, LookupEnv: func(k string) (string, bool) { v, ok := tc.vars[k]; return v, ok }}
			got, err := vault.ResolveHome(env)
			if err != nil {
				t.Fatalf("ResolveHome: %v", err)
			}
			if got != tc.want {
				t.Errorf("home = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResolveHomeFailsWithoutABase(t *testing.T) {
	cases := map[string]string{"linux": "linux", "darwin": "darwin", "windows": "windows"}
	for name, goos := range cases {
		t.Run(name, func(t *testing.T) {
			env := vault.Env{GOOS: goos, LookupEnv: func(string) (string, bool) { return "", false }}
			if got, err := vault.ResolveHome(env); err == nil {
				t.Errorf("ResolveHome = %q, want an error", got)
			}
		})
	}
}

func TestResolveHomeMakesADDNHomeAbsolute(t *testing.T) {
	env := vault.Env{GOOS: "linux", LookupEnv: func(k string) (string, bool) {
		return "relative/home", k == "ADDN_HOME"
	}}
	got, err := vault.ResolveHome(env)
	if err != nil {
		t.Fatalf("ResolveHome: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("home = %q, want an absolute path", got)
	}
}
