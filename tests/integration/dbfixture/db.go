// Package dbfixture owns only randomly named synthetic test databases.
package dbfixture

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"

	driver "github.com/go-sql-driver/mysql"
)

var databaseName = regexp.MustCompile(`^loreviva_test_[a-f0-9]{24}$`)

func Open(t *testing.T) *sql.DB {
	t.Helper()
	raw := os.Getenv("LOREVIVA_TEST_DSN")
	if raw == "" {
		t.Fatal("LOREVIVA_TEST_DSN required: integration tests must use actual isolated MySQL")
	}
	cfg, err := driver.ParseDSN(raw)
	if err != nil {
		t.Fatal("invalid private test connection")
	}
	if cfg.Net != "tcp" || cfg.Addr != "127.0.0.1:33267" || strings.EqualFold(cfg.User, "root") {
		t.Fatal("test database must be the approved loopback endpoint")
	}
	cfg.DBName = ""
	cfg.ParseTime = true
	conn, err := driver.NewConnector(cfg)
	if err != nil {
		t.Fatal("invalid test connector")
	}
	admin := sql.OpenDB(conn)
	var bytes [12]byte
	if _, err = rand.Read(bytes[:]); err != nil {
		t.Fatal(err)
	}
	name := "loreviva_test_" + hex.EncodeToString(bytes[:])
	if !databaseName.MatchString(name) {
		t.Fatal("unsafe test database name")
	}
	if _, err = admin.ExecContext(context.Background(), "CREATE DATABASE `"+name+"` CHARACTER SET utf8mb4 COLLATE utf8mb4_0900_bin"); err != nil {
		t.Fatal("cannot create isolated synthetic database", err)
	}
	cfg.DBName = name
	conn, err = driver.NewConnector(cfg)
	if err != nil {
		t.Fatal("invalid scoped connector")
	}
	db := sql.OpenDB(conn)
	t.Cleanup(func() {
		db.Close()
		if !databaseName.MatchString(name) {
			t.Error("refused unsafe cleanup")
			return
		}
		if _, err := admin.Exec("DROP DATABASE `" + name + "`"); err != nil {
			t.Error("synthetic cleanup failed", err)
		}
		admin.Close()
	})
	t.Logf("actual MySQL synthetic database %s", name)
	return db
}

// ReopenHelper is exclusively for a child OS process in the lease test.
func ReopenHelper(t *testing.T) *sql.DB {
	t.Helper()
	cfg, err := driver.ParseDSN(os.Getenv("LOREVIVA_TEST_DSN"))
	if err != nil {
		t.Fatal("invalid helper connector")
	}
	name := os.Getenv("LOREVIVA_HELPER_DATABASE")
	if !databaseName.MatchString(name) || cfg.Net != "tcp" || cfg.Addr != "127.0.0.1:33267" || strings.EqualFold(cfg.User, "root") {
		t.Fatal("unsafe helper database")
	}
	cfg.DBName = name
	cfg.ParseTime = true
	c, err := driver.NewConnector(cfg)
	if err != nil {
		t.Fatal("invalid helper connector")
	}
	db := sql.OpenDB(c)
	t.Cleanup(func() { db.Close() })
	return db
}
