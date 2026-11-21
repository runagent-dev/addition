package cli

import (
	"crypto/sha256"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

func TestDoctorOfflineLeavesTheDataDirUntouched(t *testing.T) {
	c := newCallHarness(t)
	stale := filepath.Join(c.home, ".master.key-deadbeef")
	if err := os.WriteFile(stale, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotTree(t, c.home)

	wantHealthy(t, c.run("doctor"))

	wantSameTree(t, before, snapshotTree(t, c.home))
}

func TestDoctorOfflineOnAnOlderSchemaDoesNotMigrate(t *testing.T) {
	c := newCallHarness(t)
	rawExec(t, c.home, `ALTER TABLE credentials DROP COLUMN revision; UPDATE meta SET value = '2' WHERE key = 'schema_version'`)
	before := snapshotTree(t, c.home)

	rep := wantNeedsAction(t, c.run("doctor"), "addition accounts list")

	if v := rep.check(t, "vault"); v.Status != "warn" || v.Fix != "addition accounts list" {
		t.Errorf("vault check = %+v, want a warning that any addition command upgrades the vault", v)
	}
	wantSameTree(t, before, snapshotTree(t, c.home))
}

func TestDoctorOfflineOnANewerSchemaFailsWithoutWriting(t *testing.T) {
	c := newCallHarness(t)
	rawExec(t, c.home, `UPDATE meta SET value = '999' WHERE key = 'schema_version'`)
	before := snapshotTree(t, c.home)

	rep := wantNeedsAction(t, c.run("doctor"), "upgrade addition")

	if v := rep.check(t, "vault"); v.Status != "fail" {
		t.Errorf("vault check = %+v", v)
	}
	wantSameTree(t, before, snapshotTree(t, c.home))
}

func TestDoctorWithNoHomeCreatesNothing(t *testing.T) {
	home := isolatedHome(t)
	parent := filepath.Dir(home)
	before := snapshotTree(t, parent)

	wantNeedsAction(t, run("doctor"), fixInit)

	wantSameTree(t, before, snapshotTree(t, parent))
}

func TestDoctorLiveWritesOnlyTheRefreshAndAuditRows(t *testing.T) {
	c := newCallHarness(t)
	c.expireToken()
	stale := filepath.Join(c.home, ".master.key-deadbeef")
	if err := os.WriteFile(stale, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	events := auditEvents(t, c.home)
	before := snapshotTree(t, c.home)

	wantHealthy(t, c.run("doctor", "--live"))

	after := snapshotTree(t, c.home)
	for path, was := range before {
		if !strings.HasPrefix(filepath.Base(path), "addition.db") && path != c.home && after[path] != was {
			t.Errorf("%s changed: %+v -> %+v", path, was, after[path])
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("%s was created", path)
		}
	}
	if got := auditEvents(t, c.home); !slices.Equal(got, append(events, "refresh", "call")) {
		t.Errorf("audit events = %v, want %v plus refresh and call", got, events)
	}
}
