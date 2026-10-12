package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"sort"
	"strings"
)

var rankKinds = map[int][]string{
	10: {""}, 20: {""}, 30: {""}, 40: {""}, 45: {""}, 50: {""}, 60: {""},
	70: {"supplier_global", "supplier_tenant", "platform_risk", "customer", "project", "world"},
	80: {"supplier_global", "supplier_tenant", "platform_risk", "customer", "project", "world"},
	90: {"item", "actor", "relationship", "memory", "content_project", "content_draft", "content_release", "asset", "content_check"}, 100: {""},
	110: {"events", "approved_results", "service_events", "postings", "outbox", "commit_receipts"},
}

func kindOrder(k LockKey) int {
	for i, s := range rankKinds[k.Rank] {
		if k.Kind == s {
			return i
		}
	}
	return -1
}
func less(a, b LockKey) bool {
	if a.Rank != b.Rank {
		return a.Rank < b.Rank
	}
	if kindOrder(a) != kindOrder(b) {
		return kindOrder(a) < kindOrder(b)
	}
	for i := 0; i < len(a.IDs) && i < len(b.IDs); i++ {
		if a.IDs[i] != b.IDs[i] {
			return bytes.Compare([]byte(a.IDs[i]), []byte(b.IDs[i])) < 0
		}
	}
	return len(a.IDs) < len(b.IDs)
}
func lockBytes(k LockKey) []byte {
	// Length prefixes are for collision-free identity, NOT sort order.
	b := []byte{byte(k.Rank), byte(kindOrder(k))}
	for _, id := range k.IDs {
		var size [2]byte
		binary.BigEndian.PutUint16(size[:], uint16(len(id)))
		b = append(b, size[:]...)
		b = append(b, []byte(id)...)
	}
	return b
}
func PlanLocks(input []LockKey) ([]LockKey, error) {
	p := make([]LockKey, len(input))
	for i, k := range input {
		if _, ok := rankKinds[k.Rank]; !ok || kindOrder(k) < 0 || len(k.IDs) == 0 {
			return nil, errors.New("unregistered lock rank, kind or empty address")
		}
		p[i] = LockKey{Rank: k.Rank, Kind: k.Kind, IDs: append([]string(nil), k.IDs...)}
		for _, id := range k.IDs {
			if len(id) == 0 || len(id) > 256 || strings.IndexByte(id, 0) >= 0 {
				return nil, errors.New("invalid binary lock identity")
			}
		}
		if len(lockBytes(k)) > 1024 {
			return nil, errors.New("lock address too long")
		}
	}
	sort.Slice(p, func(i, j int) bool { return less(p[i], p[j]) })
	out := p[:0]
	for _, k := range p {
		if len(out) == 0 || !bytes.Equal(lockBytes(out[len(out)-1]), lockBytes(k)) {
			out = append(out, k)
		}
	}
	return out, nil
}

// A marker INSERT itself acquires a lock, so it happens only as this exact
// position is reached in the canonical plan. Never preinsert the unsorted set.
func marker(ctx context.Context, tx *sql.Tx, k LockKey) error {
	b := lockBytes(k)
	if _, e := tx.ExecContext(ctx, "INSERT INTO lock_markers(lock_key) VALUES(?) ON DUPLICATE KEY UPDATE lock_key=lock_key", b); e != nil {
		return e
	}
	var got []byte
	return tx.QueryRowContext(ctx, "SELECT lock_key FROM lock_markers WHERE lock_key=? FOR UPDATE", b).Scan(&got)
}

type unit struct {
	skillProofs             []skillInvocationProof
	serviceMetering         *originalServiceMetering
	rehearsalHistory        *rehearsalHistoryPlan
	rehearsalReport         *rehearsalReportInspection
	rehearsalReportRead     bool
	rehearsalExecution      *rehearsalExecution
	rehearsalChildStart     *rehearsalChildStart
	rehearsalInitialization *rehearsalInitialization
	approvedHistoryRead     bool
	worldAccounting         *worldAccountingPlan
	resultStorage           string
	tx                      *sql.Tx
	plan                    map[string]bool
	err                     error
	active                  *LockKey
	scope                   domain.ProductScope
	taskID                  string
	commandOnly             bool
	fence                   uint64
	purpose                 BarrierPurpose
	world                   string
	controlVersion          uint64
	runtime                 *RuntimeBinding
	acceptedTurnID          string
	candidateSourcesOwner   DraftCandidateOwner
	rehearsalSourcesOwner   DraftRehearsalOwner
	rehearsalCleanup        *rehearsalStageClosure
	rehearsalPurged         string
}

func (u *unit) ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error) {
	if u.rehearsalReportRead || u.rehearsalHistory != nil {
		u.err = ErrDenied
		return nil, u.err
	}
	if u.err != nil {
		return nil, u.err
	}
	// This narrow local port rejects unplanned write tables. It is deliberately
	// NOT a SQL security sandbox: typed keys + trusted repository code bind rows.
	words := strings.Fields(strings.ToLower(q))
	table := ""
	if len(words) >= 3 && words[0] == "insert" && words[1] == "into" {
		table = strings.Split(words[2], "(")[0]
	} else if len(words) >= 2 && words[0] == "update" {
		table = words[1]
	} else if len(words) >= 3 && words[0] == "delete" && words[1] == "from" {
		table = words[2]
	}
	rank, kind := 0, ""
	switch table {
	case "runtime_instances":
		rank = 40
	case "runtime_tasks", "migration_plans":
		rank, kind = 90, "content_check"
	case "runtime_events":
		rank, kind = 110, "events"
	case "items":
		rank, kind = 90, "item"
	case "actors":
		rank, kind = 90, "actor"
	case "relationships":
		rank, kind = 90, "relationship"
	case "memories", "memory_aggregates", "memory_governance":
		rank, kind = 90, "memory"
	case "world_projects":
		rank, kind = 90, "content_project"
	case "content_drafts", "content_revisions":
		rank, kind = 90, "content_draft"
	case "releases", "content_releases":
		rank, kind = 90, "content_release"
	case "assets", "file_staging", "memory_asset_lineage":
		rank, kind = 90, "asset"
	case "content_checks", "admin_results", "seed_receipts":
		rank, kind = 90, "content_check"
	case "inbox":
		rank = 100
	case "events", "approved_results", "service_events", "postings", "outbox":
		rank, kind = 110, table
	}
	if rank == 0 || u.active == nil || u.active.Rank != rank || u.active.Kind != kind {
		u.err = ErrReplan
		return nil, u.err
	}
	res, e := u.tx.ExecContext(ctx, q, args...)
	if e != nil {
		u.err = e
	}
	return res, e
}

type trackedRow struct {
	row *sql.Row
	u   *unit
}
type failureRow struct{ err error }

func (r failureRow) Scan(...any) error { return r.err }

func (r trackedRow) Scan(args ...any) error {
	e := r.row.Scan(args...)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		r.u.err = e
	}
	return e
}
func (u *unit) QueryRowContext(ctx context.Context, q string, args ...any) Row {
	if u.err != nil {
		return failureRow{u.err}
	}
	// Locking reads are completed by the planner, never discovered inside body.
	normal := strings.ToLower(strings.Join(strings.Fields(q), " "))
	if !strings.HasPrefix(normal, "select ") || strings.Contains(normal, "for update") || strings.Contains(normal, "for share") || strings.Contains(normal, "lock in") || strings.Contains(normal, "get_lock(") || strings.Contains(normal, "into ") {
		u.err = ErrReplan
		return failureRow{u.err}
	}
	return trackedRow{row: u.tx.QueryRowContext(ctx, q, args...), u: u}
}
func (u *unit) RequireLock(k LockKey) error {
	if !u.plan[string(lockBytes(k))] {
		u.err = ErrReplan
		return ErrReplan
	}
	copy := k
	u.active = &copy
	return nil
}
