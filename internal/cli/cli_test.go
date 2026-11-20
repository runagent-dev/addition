package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

type result struct {
	code   int
	stdout string
	stderr string
}

func run(args ...string) result {
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return result{code: code, stdout: stdout.String(), stderr: stderr.String()}
}

type errorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeError(t *testing.T, stderr string) errorEnvelope {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal([]byte(stderr), &env); err != nil {
		t.Fatalf("stderr is not a JSON error envelope: %v\nstderr: %q", err, stderr)
	}
	return env
}

func TestVersionPrintsJSON(t *testing.T) {
	r := run("version")

	if r.code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", r.code, ExitOK, r.stderr)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q, want empty", r.stderr)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\nstdout: %q", err, r.stdout)
	}
	if got["version"] != Version {
		t.Errorf("version = %q, want %q", got["version"], Version)
	}
}

func TestUsageErrorsUseEnvelopeAndExitCode(t *testing.T) {
	cases := map[string][]string{
		"unknown command": {"bogus"},
		"unknown flag":    {"version", "--nope"},
		"extra argument":  {"version", "extra"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r := run(args...)

			if r.code != ExitUsage {
				t.Fatalf("exit code = %d, want %d", r.code, ExitUsage)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
			env := decodeError(t, r.stderr)
			if env.Error.Code != "usage" {
				t.Errorf("error.code = %q, want %q", env.Error.Code, "usage")
			}
			if env.Error.Message == "" {
				t.Error("error.message is empty")
			}
		})
	}
}

func TestExitCodesMatchThePlan(t *testing.T) {
	want := map[string]int{
		"ok": 0, "usage": 2, "needs-connect": 3, "upstream-4xx": 4, "upstream-5xx": 5, "policy-denied": 6,
	}
	got := map[string]int{
		"ok": ExitOK, "usage": ExitUsage, "needs-connect": ExitNeedsConnect,
		"upstream-4xx": ExitUpstreamClient, "upstream-5xx": ExitUpstreamServer, "policy-denied": ExitPolicyDenied,
	}
	for name, code := range want {
		if got[name] != code {
			t.Errorf("%s = %d, want %d", name, got[name], code)
		}
	}
}

func TestNoArgsPrintsHelpAndSucceeds(t *testing.T) {
	r := run()

	if r.code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", r.code, ExitOK, r.stderr)
	}
	if !bytes.Contains([]byte(r.stdout), []byte("addn")) {
		t.Errorf("help output does not mention addn: %q", r.stdout)
	}
}
