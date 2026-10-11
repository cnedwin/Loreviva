package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"path/filepath"
	"strings"
)

type Local struct {
	Coordinator *maintenance.Coordinator
	Participant *maintenance.Participant
	Ledger      *memory.DeletionLedger
}

func bindings(ctx context.Context, s *store.Store, root, files string) (string, string, string, error) {
	a, e := filepath.Abs(root)
	if e != nil {
		return "", "", "", e
	}
	f, e := filepath.Abs(files)
	if e != nil {
		return "", "", "", e
	}
	rel, e := filepath.Rel(f, a)
	if e != nil || rel == "." || !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", "", maintenance.ErrMaintenance
	}
	rev, e := filepath.Rel(a, f)
	if e != nil || rev == "." || !strings.HasPrefix(rev, ".."+string(filepath.Separator)) {
		return "", "", "", maintenance.ErrMaintenance
	}
	var db string
	if e = s.DB.QueryRowContext(ctx, "SELECT DATABASE()").Scan(&db); e != nil {
		return "", "", "", e
	}
	return a, content.Identity("database", db), content.Identity("files", f), nil
}

// InitializeLocal is only for an explicit empty, fresh synthetic installation.
// Partial file/DB setup stays closed; no missing independent file is recreated.
func InitializeLocal(ctx context.Context, s *store.Store, root, files string) error {
	root, db, fb, e := bindings(ctx, s, root, files)
	if e != nil {
		return e
	}
	var count int
	if e = s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_installation").Scan(&count); e != nil {
		return e
	}
	if count != 0 {
		local, e := OpenLocal(ctx, s, root, files)
		if e != nil {
			return e
		}
		return local.Participant.Close(ctx)
	}
	var empty bool
	if e = s.DB.QueryRowContext(ctx, "SELECT (SELECT COUNT(*) FROM tenant_gates)=0 AND (SELECT COUNT(*) FROM runtime_instances)=0 AND (SELECT COUNT(*) FROM content_drafts)=0 AND (SELECT COUNT(*) FROM tasks)=0 AND (SELECT COUNT(*) FROM assets)=0 AND (SELECT COUNT(*) FROM commit_receipts)=0").Scan(&empty); e != nil || !empty {
		return maintenance.ErrMaintenance
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return e
	}
	id := hex.EncodeToString(nonce[:])
	if _, e = maintenance.CreateCoordinator(filepath.Join(root, "coordination"), id, db, fb); e != nil {
		return e
	}
	if _, e = memory.CreateLedger(filepath.Join(root, "ledger"), id, "governance", "trusted-anchor"); e != nil {
		return e
	}
	_, e = s.DB.ExecContext(ctx, "INSERT INTO memory_installation VALUES(1,?,1,?,?,?)", id, db, fb, localstate.RootHash(filepath.Join(root, "coordination")))
	return e
}
func OpenLocal(ctx context.Context, s *store.Store, root, files string) (*Local, error) {
	root, db, fb, e := bindings(ctx, s, root, files)
	if e != nil {
		return nil, e
	}
	var id, recordDB, recordFiles, recordCoordinator string
	var generation uint64
	if e = s.DB.QueryRowContext(ctx, "SELECT installation_id,generation,database_binding,files_binding,coordinator_binding FROM memory_installation WHERE singleton=1").Scan(&id, &generation, &recordDB, &recordFiles, &recordCoordinator); e != nil || db != recordDB || fb != recordFiles || recordCoordinator != localstate.RootHash(filepath.Join(root, "coordination")) {
		return nil, maintenance.ErrMaintenance
	}
	c, e := maintenance.OpenCoordinator(filepath.Join(root, "coordination"), id, db, fb)
	if e != nil {
		return nil, e
	}
	if e = c.CheckOpenGeneration(ctx, generation); e != nil {
		return nil, e
	}
	l, e := memory.OpenLedger(filepath.Join(root, "ledger"), id, "governance", "trusted-anchor")
	if e != nil {
		return nil, e
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	p, e := c.Register(ctx, "process-"+hex.EncodeToString(nonce[:]), []maintenance.Writer{maintenance.APIWriter, maintenance.StoreWriter, maintenance.WorkerWriter, maintenance.FileWriter, maintenance.GovernanceWriter})
	if e != nil {
		return nil, e
	}
	s.Guard = p
	return &Local{c, p, l}, nil
}
