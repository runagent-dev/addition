//go:build !windows

package cli

import (
	"os/exec"
	"testing"
)

func TestLauncherReportsHowTheBrowserCommandExited(t *testing.T) {
	cases := map[string]struct {
		cmd     *exec.Cmd
		wantErr bool
	}{
		"exits 0":      {exec.Command("sh", "-c", "exit 0"), false},
		"exits 3":      {exec.Command("sh", "-c", "exit 3"), true},
		"cannot start": {exec.Command("/nonexistent/addition-test-launcher"), true},
	}
	for name, tc := range cases {
		if err := <-launch(tc.cmd); (err != nil) != tc.wantErr {
			t.Errorf("%s: launch err = %v, want error %v", name, err, tc.wantErr)
		}
	}
}
