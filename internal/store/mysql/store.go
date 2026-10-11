// Package mysql implements repository-internal authority on a MySQL primary.
package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"loreviva.local/loreviva/internal/config"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/migrations"
)

var ErrNoTask = errors.New("no eligible task")
var ErrCommitUnknown = errors.New("local commit unknown; query original receipt")
var ErrDenied = errors.New("current scope, ownership or barrier denied")
var ErrConflict = errors.New("stable identity conflicts with existing request")
var ErrReplan = errors.New("lock absent from frozen plan; rollback and replan whole transaction")

// Commit is a transport seam: nil uses database/sql Commit. Any returned error is
// conservative UNKNOWN. It may not perform provider/network work. BeforeCommit
// is an optional local pre-commit validation seam and always rolls back on error.
type Store struct {
	Guard        OperationGuard
	DB           *sql.DB
	BeforeCommit func(context.Context, *sql.Tx) error
	Commit       func(context.Context, *sql.Tx) error
	Trace        func(LockKey)
}
type OperationGuard interface {
	EnterStore(context.Context) (context.Context, func(), error)
}

// EnterOperation is attached by the executable before serving or dispatching.
// A nil guard exists only for isolated repository tests and pre-runtime setup.
func (s *Store) EnterOperation(ctx context.Context) (context.Context, func(), error) {
	if s.Guard == nil {
		return ctx, func() {}, nil
	}
	return s.Guard.EnterStore(ctx)
}

type Tx interface {
	// Only local, bounded SQL is allowed. Modules must RequireLock for every
	// precollected business row before issuing its SQL; this is a trusted module
	// port, never an HTTP/general SQL endpoint. The callback MUST NOT do network I/O.
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) Row
	RequireLock(LockKey) error
	CreateTask(context.Context, domain.TaskIntake) error
	CreateRegisteredTask(context.Context, domain.TaskIntake, string) error
	CancelRegisteredTask(context.Context, string) error
	SetTaskState(context.Context, string) error
	CreateFileCleanup(context.Context, string, time.Time, uint64) error
	AdvanceFileCleanup(context.Context, uint64, bool) error
	SetControlStatus(context.Context, string) error
	CreateWorld(context.Context, string) error
	AdvanceWorld(context.Context, string, string) error
	SetMigration(context.Context, string, bool) error
	AssertWorldTask(domain.Scope, string) error
	CancelRuntimeMigration(context.Context, string, uint64) error
	AdvanceMemoryPrivacy(context.Context, string) error
}
type Row interface{ Scan(...any) error }
type GateVersions struct{ Tenant, Product, Subject, Privacy, View uint64 }
type BarrierPurpose string

const (
	ExecutionPurpose  BarrierPurpose = ""
	ControlPurpose    BarrierPurpose = "CONTROL"
	GovernancePurpose BarrierPurpose = "GOVERNANCE"
	MigrationPurpose  BarrierPurpose = "MIGRATION"
)

type BarrierRequest struct {
	Runtime                                                            *RuntimeBinding
	ProductScope                                                       domain.ProductScope
	WorldScope                                                         *domain.Scope
	Versions                                                           *domain.Versions
	OwnerSubjectID, Operation, CommandKey, CommitKey, TaskID, WorkerID string
	Fence                                                              uint64
	GateVersions                                                       GateVersions
	PayloadHash                                                        [32]byte
	Locks                                                              []LockKey
	phase                                                              string
	commandOnly                                                        bool
	Purpose                                                            BarrierPurpose
	ExpectedWorldStatus                                                string
}
type CommandBarrierRequest struct {
	Runtime                                          *RuntimeBinding
	ProductScope                                     domain.ProductScope
	WorldScope                                       *domain.Scope
	Versions                                         *domain.Versions
	Purpose                                          BarrierPurpose
	ExpectedWorldStatus                              string
	OwnerSubjectID, Operation, CommandKey, CommitKey string
	GateVersions                                     GateVersions
	PayloadHash                                      [32]byte
	Locks                                            []LockKey
}
type CommitReceipt struct {
	Key, TaskID, OwnerSubjectID string
	Committed                   bool
}
type TaskLease = domain.TaskLease
type LockKey struct {
	Rank int
	Kind string
	IDs  []string
}
type Reservation struct {
	AccountID, Kind, ScopeKey, ReservationID string
	Amount                                   int64
}
type DispatchRequest struct {
	BarrierRequest
	AttemptID, AuthorizationID, Route, RequestKey string
	RequestHash                                   [32]byte
	Slot                                          uint8
	Reservations                                  []Reservation
}
type DispatchAuthorization struct {
	AttemptID              string
	Receipt                CommitReceipt
	CanSend, QueryOriginal bool
}

func New(db *sql.DB) *Store { return &Store{DB: db} }
func Open(c config.Database) (*Store, error) {
	cfg := driver.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = c.Addr
	cfg.DBName = c.Name
	cfg.User = c.User
	cfg.Passwd = c.Password.Reveal()
	cfg.ParseTime = true
	cfg.Loc = time.UTC
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 10 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	conn, err := driver.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("invalid database connector configuration")
	}
	return New(sql.OpenDB(conn)), nil
}
func (s *Store) Close() error { return s.DB.Close() }

// Migrate is an explicit operator/startup migration action; readiness never
// creates gates, users or synthetic seed. MySQL DDL is not transaction atomic:
// idempotent statements allow retry, and the final hash marker proves completion.
func (s *Store) Migrate(ctx context.Context) error {
	conn, e := s.DB.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	var obtained int
	if e = conn.QueryRowContext(ctx, "SELECT GET_LOCK('loreviva.core.schema',10)").Scan(&obtained); e != nil {
		return e
	}
	if obtained != 1 {
		return errors.New("schema migration lock unavailable")
	}
	defer conn.ExecContext(context.WithoutCancel(ctx), "SELECT RELEASE_LOCK('loreviva.core.schema')")
	for version, source := range []string{migrations.Core, migrations.Content, migrations.FileCleanup, migrations.Runtime, migrations.MemoryGovernance} {
		sum := sha256.Sum256([]byte(source))
		var existing []byte
		e = conn.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version=?", version+1).Scan(&existing)
		if e == nil {
			if string(existing) != string(sum[:]) {
				return errors.New("schema migration hash mismatch")
			}
			continue
		}
		// Only a completely fresh database may lack the migration table.
		if !errors.Is(e, sql.ErrNoRows) {
			var my *driver.MySQLError
			if !errors.As(e, &my) || my.Number != 1146 {
				return errors.New("cannot verify migration state")
			}
		}
		for _, statement := range strings.Split(source, ";") {
			if strings.TrimSpace(statement) == "" {
				continue
			}
			if version == 3 && strings.HasPrefix(strings.TrimSpace(statement), "ALTER TABLE world_controls") {
				if e = migrateInitialControl(ctx, conn, statement); e != nil {
					return e
				}
				continue
			}
			if _, e = conn.ExecContext(ctx, statement); e != nil {
				return fmt.Errorf("core migration failed: %w", e)
			}
		}
		if _, e = conn.ExecContext(ctx, "INSERT INTO schema_migrations(version,sha256) VALUES(?,?)", version+1, sum[:]); e != nil {
			return e
		}
	}
	return nil
}

// Readiness verifies persistence only, then each declared prerequisite. No
// prerequisites means fail closed: future files/seed/governance cannot be assumed.
func (s *Store) Readiness(ctx context.Context, prerequisites ...func(context.Context) error) error {
	if e := s.DB.PingContext(ctx); e != nil {
		return errors.New("database primary unreachable")
	}
	sum := sha256.Sum256([]byte(migrations.Core))
	var got []byte
	if e := s.DB.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version=1").Scan(&got); e != nil || string(got) != string(sum[:]) {
		return errors.New("required core migration not verified")
	}
	if len(prerequisites) == 0 {
		return errors.New("seed, private files and governance prerequisites not registered")
	}
	for _, check := range prerequisites {
		if check == nil {
			return errors.New("nil startup prerequisite")
		}
		if e := check(ctx); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) finish(ctx context.Context, tx *sql.Tx, r BarrierRequest) error {
	if s.BeforeCommit != nil {
		if e := s.BeforeCommit(ctx, tx); e != nil {
			return e
		}
	}
	if e := checkCurrentLease(ctx, tx, r); e != nil {
		return e
	}
	commit := s.Commit
	if commit == nil {
		commit = func(_ context.Context, t *sql.Tx) error { return t.Commit() }
	}
	if e := commit(ctx, tx); e != nil {
		return ErrCommitUnknown
	}
	return nil
}
