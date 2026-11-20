package vault

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWriterLockOutlivesCallerCancellation(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, dbFile), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	holder, err := openDB(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	contender, err := openDB(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer contender.Close()
	conn, err := contender.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "PRAGMA busy_timeout = 0"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	err = withWriterLock(ctx, holder, func() error {
		cancel()
		// database/sql rolls a cancelled transaction back asynchronously, so probe for a while.
		for deadline := time.Now().Add(300 * time.Millisecond); time.Now().Before(deadline); {
			if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err == nil {
				_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
				t.Fatal("writer lock was released while its holder was still running")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
