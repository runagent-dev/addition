package cli

import (
	"crypto/sha256"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type fileState struct {
	mode  fs.FileMode
	mtime time.Time
	size  int64
	sum   [32]byte
}

// snapshotTree records every entry under dir; a missing dir snapshots as empty.
func snapshotTree(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	out := map[string]fileState{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		state := fileState{mode: info.Mode(), mtime: info.ModTime(), size: info.Size()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			state.sum = sha256.Sum256(data)
		}
		out[path] = state
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return out
}

func wantSameTree(t *testing.T, before, after map[string]fileState) {
	t.Helper()
	for path, was := range before {
		if now, ok := after[path]; !ok {
			t.Errorf("%s was removed", path)
		} else if now != was {
			t.Errorf("%s changed: %+v -> %+v", path, was, now)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was created", path)
		}
	}
}

func rawExec(t *testing.T, home, statements string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "addition.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(statements); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDoctorWithNoHomeCreatesNothing(t *testing.T) {
	home := isolatedHome(t)
	parent := filepath.Dir(home)
	before := snapshotTree(t, parent)

	wantNeedsAction(t, run("doctor"), fixInit)

	wantSameTree(t, before, snapshotTree(t, parent))
}

func TestDoctorWithAHomeButNoDatabaseCreatesNothing(t *testing.T) {
	home := isolatedHome(t)
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, home)

	rep := wantNeedsAction(t, run("doctor"), fixInit)

	if c := rep.check(t, "vault"); c.Status != "fail" || c.Fix != fixInit {
		t.Errorf("vault check = %+v", c)
	}
	wantSameTree(t, before, snapshotTree(t, home))
}

func TestDoctorWithAnUnreadableHomeFailsClearly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads any directory")
	}
	c := newCallHarness(t)
	if err := os.Chmod(c.home, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(c.home, 0o700) })

	r := c.run("doctor")

	rep := wantNeedsAction(t, r, "make the data dir readable by your user only (chmod 700)")
	if v := rep.check(t, "vault"); v.Status != "fail" || v.Summary == "" {
		t.Errorf("vault check = %+v", v)
	}
}

func TestDoctorOnAnOlderSchemaUpgradesItLikeAnyCommand(t *testing.T) {
	c := newCallHarness(t)
	rawExec(t, c.home, `ALTER TABLE credentials DROP COLUMN revision; UPDATE meta SET value = '2' WHERE key = 'schema_version'`)

	wantHealthy(t, c.run("doctor"))
}

func TestDoctorOnANewerSchemaFails(t *testing.T) {
	c := newCallHarness(t)
	rawExec(t, c.home, `UPDATE meta SET value = '999' WHERE key = 'schema_version'`)

	rep := wantNeedsAction(t, c.run("doctor"), "upgrade addition")

	if v := rep.check(t, "vault"); v.Status != "fail" {
		t.Errorf("vault check = %+v", v)
	}
}

func TestDoctorIgnoresAnEmptyJournalLeftBesideTheDatabase(t *testing.T) {
	c := newCallHarness(t)
	if err := os.WriteFile(filepath.Join(c.home, "addition.db-journal"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	wantHealthy(t, c.run("doctor"))
}

func TestDoctorLiveWritesOnlyTheRefreshAndAuditRows(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()
	events := auditEvents(t, c.home)

	wantHealthy(t, c.run("doctor", "--live"))

	if got := auditEvents(t, c.home); !slices.Equal(got, append(events, "refresh", "call")) {
		t.Errorf("audit events = %v, want %v plus refresh and call", got, events)
	}
}
