package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

type result struct {
	args   []string
	code   int
	stdout string
	stderr string
}

// command names the invocation by its first words only, since later arguments may hold codes.
func (r result) command() string {
	return "addn " + strings.Join(r.args[:min(2, len(r.args))], " ")
}

func run(args ...string) result {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), args, &stdout, &stderr)
	return result{args: args, code: code, stdout: stdout.String(), stderr: stderr.String()}
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
		"ok": 0, "internal": 1, "usage": 2, "needs-connect": 3, "upstream-4xx": 4, "upstream-5xx": 5, "policy-denied": 6,
	}
	got := map[string]int{
		"ok": ExitOK, "internal": ExitInternal, "usage": ExitUsage, "needs-connect": ExitNeedsConnect,
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

func TestMalformedRequestsAreUsageErrors(t *testing.T) {
	cases := map[string][]string{
		"help for unknown command":  {"help", "bogus"},
		"help with extra argument":  {"help", "version", "extra"},
		"completion is not offered": {"completion", "bash"},
		"empty argument":            {""},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			r := run(args...)

			if r.code != ExitUsage {
				t.Fatalf("exit code = %d, want %d (stdout: %q)", r.code, ExitUsage, r.stdout)
			}
			if r.stdout != "" {
				t.Errorf("stdout = %q, want empty", r.stdout)
			}
			if env := decodeError(t, r.stderr); env.Error.Code != "usage" {
				t.Errorf("error.code = %q, want usage", env.Error.Code)
			}
		})
	}
}

func TestHelpRequestsSucceed(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"-h"}, {"help"}, {"help", "version"}, {"version", "--help"}} {
		r := run(args...)
		if r.code != ExitOK || r.stderr != "" {
			t.Errorf("%v: exit %d, stderr %q", args, r.code, r.stderr)
		}
		if !strings.Contains(r.stdout, "Usage:") {
			t.Errorf("%v: stdout has no usage text: %q", args, r.stdout)
		}
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("no space left on device") }

func TestStdoutWriteFailureIsAnOutputError(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--help"}} {
		var stderr bytes.Buffer
		code := Run(context.Background(), args, brokenWriter{}, &stderr)

		if code != ExitInternal {
			t.Errorf("%v: exit code = %d, want %d", args, code, ExitInternal)
		}
		if env := decodeError(t, stderr.String()); env.Error.Code != "output" {
			t.Errorf("%v: error.code = %q, want output", args, env.Error.Code)
		}
	}
}

func TestNilArgsDoNotReadProcessArgs(t *testing.T) {
	saved := os.Args
	t.Cleanup(func() { os.Args = saved })
	os.Args = []string{"addn.test", "bogus"}

	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), nil, &stdout, &stderr); code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}
}

func TestHelpTopicMatchesHelpFlag(t *testing.T) {
	topic, flag := run("help", "version"), run("version", "--help")

	if topic.stdout != flag.stdout {
		t.Errorf("help version and version --help differ:\n--- topic\n%s\n--- flag\n%s", topic.stdout, flag.stdout)
	}
}

func TestNilWritersFallBackToProcessStreams(t *testing.T) {
	var stderr bytes.Buffer
	if code := Run(context.Background(), []string{"version"}, nil, &stderr); code != ExitOK {
		t.Fatalf("exit code = %d, want %d (stderr: %q)", code, ExitOK, stderr.String())
	}
}

func TestHiddenCompletionEntryPointsAreRejected(t *testing.T) {
	cases := [][]string{
		{"__complete", "--bogus", ""},
		{"__completeNoDesc", "version", ""},
		{"--help=false", "__complete", "version", ""},
	}
	for _, args := range cases {
		r := run(args...)

		if r.code != ExitUsage || r.stdout != "" {
			t.Errorf("%v: exit %d, stdout %q", args, r.code, r.stdout)
		}
		decodeError(t, r.stderr)
	}
}
