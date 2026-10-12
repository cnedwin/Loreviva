package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/harness"
	"loreviva.local/loreviva/internal/localstate"
	"loreviva.local/loreviva/internal/maintenance"
	"loreviva.local/loreviva/internal/memory"
	store "loreviva.local/loreviva/internal/store/mysql"
	"path/filepath"
	"strings"
)

type Local struct {
	Mock        *harness.MockJournal
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
	coordinator, e := maintenance.CreateCoordinator(filepath.Join(root, "coordination"), id, db, fb)
	if e != nil {
		return e
	}
	if _, e = memory.CreateLedger(filepath.Join(root, "ledger"), id, "governance", "trusted-anchor"); e != nil {
		return e
	}
	if e = coordinator.InitializeGovernanceJournal(ctx, localstate.RootHash(filepath.Join(root, "ledger")), "governance", "trusted-anchor"); e != nil {
		return e
	}
	if e = harness.CreateMockJournal(filepath.Join(root, "mock"), id); e != nil {
		return e
	}
	if e = coordinator.InitializeMockJournal(ctx, localstate.RootHash(filepath.Join(root, "mock"))); e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "INSERT INTO memory_installation VALUES(1,?,1,?,?,?)", id, db, fb, localstate.RootHash(filepath.Join(root, "coordination"))); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_query_services(installation_id,service_id,action,policy,enabled) VALUES(?,'r0-mock-original-query','attempt.cost.query','R0_ORIGINAL_MOCK_COST_V1',1)", id); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_settlement_services VALUES(?,'r0-mock-settlement','attempt.cost.settle','R0_BOUNDED_SETTLEMENT_V1',1)", id); e != nil {
		return e
	}
	genesis := domain.MockGenesis(id, localstate.RootHash(filepath.Join(root, "mock")))
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_bill_cursor VALUES(?,0,?,'')", id, genesis.Hash); e != nil {
		return e
	}
	if s.Commit != nil {
		e = s.Commit(ctx, tx)
	} else {
		e = tx.Commit()
	}
	if e != nil {
		return store.ErrCommitUnknown
	}
	return nil
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
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return nil, e
	}
	p, e := c.Register(ctx, "process-"+hex.EncodeToString(nonce[:]), []maintenance.Writer{maintenance.APIWriter, maintenance.StoreWriter, maintenance.WorkerWriter, maintenance.FileWriter, maintenance.GovernanceWriter, maintenance.HarnessWriter, maintenance.MockWriter, maintenance.QueryWriter, maintenance.CleanupWriter, maintenance.DeadlineWriter})
	if e != nil {
		return nil, e
	}
	l, e := memory.OpenBoundLedger(ctx, filepath.Join(root, "ledger"), id, "governance", "trusted-anchor", p)
	if e != nil {
		_ = p.Close(ctx)
		return nil, e
	}
	j, e := harness.OpenMockJournal(ctx, filepath.Join(root, "mock"), p)
	if e != nil {
		_ = p.Close(ctx)
		return nil, e
	}
	var service bool
	if e = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_query_services WHERE installation_id=? AND service_id='r0-mock-original-query' AND action='attempt.cost.query' AND policy='R0_ORIGINAL_MOCK_COST_V1')", id).Scan(&service); e != nil || !service {
		_ = p.Close(ctx)
		return nil, maintenance.ErrMaintenance
	}
	if e = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_settlement_services WHERE installation_id=? AND service_id='r0-mock-settlement' AND action='attempt.cost.settle' AND policy='R0_BOUNDED_SETTLEMENT_V1')", id).Scan(&service); e != nil || !service {
		_ = p.Close(ctx)
		return nil, maintenance.ErrMaintenance
	}
	if e = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_bill_cursor WHERE installation_id=?)", id).Scan(&service); e != nil || !service {
		p.Close(ctx)
		return nil, maintenance.ErrMaintenance
	}
	s.WorldAccountingAuthority = p
	s.SettlementAuthority = p
	s.Guard = p
	s.QueryAuthority = p
	s.ProviderEvidence = j
	return &Local{Coordinator: c, Participant: p, Ledger: l, Mock: j}, nil
}
