package proxy_test

import (
	"database/sql"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

func (f *vaultFixture) rawDB() *sql.DB {
	f.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(os.Getenv("ADDITION_HOME"), "addition.db"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = db.Close() })
	return db
}
