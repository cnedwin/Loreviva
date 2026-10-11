package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/localstate"
	"reflect"
	"strconv"
)

type RejectedCommand struct {
	request BarrierRequest
	cause   error
}

func (e *RejectedCommand) Error() string { return e.cause.Error() }
func (e *RejectedCommand) Unwrap() error { return e.cause }

type RejectionProof struct {
	installation string
	generation   uint64
	request      BarrierRequest
	task         string
}

func (p *RejectionProof) Matches(installation, task, hash string) bool {
	return p != nil && p.installation == installation && p.generation > 0 && p.task == task && hex.EncodeToString(p.request.PayloadHash[:]) == hash && p.request.Operation == "MEMORY_GOVERNANCE_ACCEPT"
}

type installationGuard interface {
	CurrentInstallation(context.Context) (string, uint64, error)
}

// ProveMemoryRejection accepts only a barrier-created definite rollback token.
// Caller holds the single governance-intake lock outside this short proof read.
func (s *Store) ProveMemoryRejection(ctx context.Context, failed error, task string) (*RejectionProof, error) {
	var rejected *RejectedCommand
	if !errors.As(failed, &rejected) {
		return nil, ErrDenied
	}
	guard, ok := s.Guard.(installationGuard)
	if !ok {
		return nil, ErrDenied
	}
	installation, generation, e := guard.CurrentInstallation(ctx)
	if e != nil {
		return nil, e
	}
	r := rejected.request
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var id string
	var dbGeneration uint64
	if e = tx.QueryRowContext(ctx, "SELECT installation_id,generation FROM memory_installation WHERE singleton=1").Scan(&id, &dbGeneration); e != nil || id != installation || dbGeneration != generation {
		return nil, ErrDenied
	}
	// Wait for any original unreturned transaction's rank45 marker to release.
	kind, _, world := scopeOf(r)
	key := LockKey{Rank: 45, IDs: []string{r.ProductScope.TenantID, r.ProductScope.ProductID, r.ProductScope.SubjectID, r.Operation, kind, world, r.CommandKey}}
	var markerBytes []byte
	e = tx.QueryRowContext(ctx, "SELECT lock_key FROM lock_markers WHERE lock_key=? FOR UPDATE", lockBytes(key)).Scan(&markerBytes)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	var accepted bool
	e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?) OR EXISTS(SELECT 1 FROM commit_receipts WHERE commit_key=?)", r.ProductScope.TenantID, r.ProductScope.ProductID, task, r.CommitKey).Scan(&accepted)
	if e != nil {
		return nil, e
	}
	if accepted {
		return nil, ErrDenied
	}
	if e = tx.Rollback(); e != nil {
		return nil, e
	}
	return &RejectionProof{installation, generation, r, task}, nil
}

// AdvanceMemoryPrivacy is deny-only and belongs to the already frozen plan.
func (u *unit) AdvanceMemoryPrivacy(ctx context.Context, subject string) error {
	if u.purpose != GovernancePurpose || u.world == "" {
		u.err = ErrDenied
		return u.err
	}
	if e := u.RequireLock(LockKey{Rank: 30, IDs: []string{u.scope.TenantID, u.scope.ProductID, subject}}); e != nil {
		return e
	}
	_, e := u.tx.ExecContext(ctx, "UPDATE subject_governance SET privacy_epoch=privacy_epoch+1,view_epoch=view_epoch+1 WHERE tenant_id=? AND product_id=? AND subject_id=?", u.scope.TenantID, u.scope.ProductID, subject)
	if e != nil {
		u.err = e
		return e
	}
	if e = u.RequireLock(LockKey{Rank: 40, IDs: []string{u.scope.TenantID, u.scope.ProductID, u.world}}); e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "UPDATE world_controls SET privacy_epoch=privacy_epoch+1,view_epoch=view_epoch+1 WHERE tenant_id=? AND product_id=? AND world_instance_id=?", u.scope.TenantID, u.scope.ProductID, u.world)
	if e != nil {
		u.err = e
	}
	return e
}

// MemoryReplayWrite is a fixed deny-only mutation. The memory module supplies
// it only while holding the concrete coordinator's exclusive ClosedPermit.
// This port cannot add records, grants, acquisitions, rights, facts or budgets.
type MemoryReplayAsset struct {
	ID, Storage string
	Delete      bool
}
type MemoryReplayWrite struct {
	Permit                            *MemoryReplayPermit
	Assets                            []MemoryReplayAsset
	Scope                             domain.Scope
	Subject, Installation, Task, Hash string
	Generation, Sequence              uint64
	Expected                          [32]byte
	Document                          []byte
	NeedsReplacement                  bool
}

func (s *Store) ReplayMemoryDeny(ctx context.Context, r MemoryReplayWrite) error {
	binding, release, e := r.Permit.pin()
	if e != nil {
		return ErrDenied
	}
	defer release()
	if binding.Installation != r.Installation || binding.Generation != r.Generation {
		return ErrDenied
	}
	if r.Scope.Validate() != nil || domain.ValidateID(r.Subject) != nil || domain.ValidateID(r.Installation) != nil || r.Generation < 2 || r.Sequence == 0 || len(r.Hash) != 64 || len(r.Document) > 16<<20 || len(r.Assets) > 1000 {
		return ErrDenied
	}
	var aggregate string
	if e := s.DB.QueryRowContext(ctx, "SELECT aggregate_id FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, r.Subject).Scan(&aggregate); e != nil {
		return e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var installation string
	var generation uint64
	if e = validateReplayInstallation(ctx, tx, binding, &installation, &generation); e != nil {
		return e
	}
	var oldHash, state string
	e = tx.QueryRowContext(ctx, "SELECT ledger_hash,state FROM memory_replay WHERE installation_id=? AND ledger_sequence=?", r.Installation, r.Sequence).Scan(&oldHash, &state)
	if e == nil {
		if oldHash != r.Hash {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	keys := []LockKey{{Rank: 30, IDs: []string{r.Scope.TenantID, r.Scope.ProductID, r.Subject}}, {Rank: 40, IDs: []string{r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID}}, {Rank: 90, Kind: "memory", IDs: []string{r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, aggregate}}}
	for _, asset := range r.Assets {
		keys = append(keys, LockKey{Rank: 90, Kind: "asset", IDs: []string{r.Scope.TenantID, r.Scope.ProductID, "staging." + asset.Storage}})
		if asset.ID != "" {
			keys = append(keys, LockKey{Rank: 90, Kind: "asset", IDs: []string{r.Scope.TenantID, r.Scope.ProductID, asset.ID}})
		}
	}
	keys, e = PlanLocks(keys)
	if e != nil {
		return e
	}
	for _, k := range keys {
		if e = marker(ctx, tx, k); e != nil {
			return e
		}
	}
	var old []byte
	var currentAggregate string
	if e = tx.QueryRowContext(ctx, "SELECT aggregate_id,document FROM memory_aggregates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=? FOR UPDATE", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, r.Subject).Scan(&currentAggregate, &old); e != nil {
		return e
	}
	if currentAggregate != aggregate || sha256.Sum256(old) != r.Expected || !denyOnlyMemory(old, r.Document) {
		return ErrDenied
	}
	if _, e = tx.ExecContext(ctx, "UPDATE memory_aggregates SET document=?,revision=revision+1 WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", r.Document, r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, r.Subject); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE subject_governance SET privacy_epoch=privacy_epoch+1,view_epoch=view_epoch+1 WHERE tenant_id=? AND product_id=? AND subject_id=?", r.Scope.TenantID, r.Scope.ProductID, r.Subject); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE world_controls SET privacy_epoch=privacy_epoch+1,view_epoch=view_epoch+1 WHERE tenant_id=? AND product_id=? AND world_instance_id=?", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID); e != nil {
		return e
	}
	var assetCount int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=?", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, r.Subject).Scan(&assetCount); e != nil || assetCount != len(r.Assets) {
		return ErrReplan
	}
	for _, asset := range r.Assets {
		var id string
		if e = tx.QueryRowContext(ctx, "SELECT COALESCE(asset_id,'') FROM memory_asset_lineage WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=? AND storage_key=?", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, r.Subject, asset.Storage).Scan(&id); e != nil || id != asset.ID {
			return ErrReplan
		}
		if !asset.Delete {
			continue
		}
		if _, e = tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET delete_intent=1,purged=0 WHERE tenant_id=? AND product_id=? AND storage_key=?", r.Scope.TenantID, r.Scope.ProductID, asset.Storage); e != nil {
			return e
		}
		if id != "" {
			if _, e = tx.ExecContext(ctx, "UPDATE assets SET state='DELETED' WHERE tenant_id=? AND product_id=? AND asset_id=?", r.Scope.TenantID, r.Scope.ProductID, id); e != nil {
				return e
			}
		}
	}
	state = "CONFIRMED"
	if r.NeedsReplacement {
		state = "BLOCKED_NEEDS_REPLACEMENT"
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO memory_replay VALUES(?,?,?,?,?)", r.Installation, r.Sequence, r.Hash, r.Task, state); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE memory_installation SET generation=? WHERE singleton=1", r.Generation); e != nil {
		return e
	}
	return s.finish(ctx, tx, BarrierRequest{commandOnly: true})
}
func denyOnlyMemory(old, new []byte) bool {
	var a, b map[string]any
	decode := func(raw []byte, out any) error {
		d := json.NewDecoder(bytes.NewReader(raw))
		d.UseNumber()
		return d.Decode(out)
	}
	if decode(old, &a) != nil || decode(new, &b) != nil {
		return false
	}
	subset := func(v, w any) bool {
		xs, _ := v.([]any)
		ys, _ := w.([]any)
		for _, x := range xs {
			found := false
			for _, y := range ys {
				if reflect.DeepEqual(x, y) {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	}
	for _, collection := range []string{"Sources", "Nodes"} {
		am, ok := a[collection].(map[string]any)
		if !ok {
			return false
		}
		bm, ok := b[collection].(map[string]any)
		if !ok || len(am) > len(bm) {
			return false
		}
		for id, bv := range bm {
			if _, exists := am[id]; exists {
				continue
			}
			row, ok := bv.(map[string]any)
			if !ok || row["Blocked"] != true || row["Body"] != "" || domain.ValidateID(id) != nil || row["ID"] != id || !subset(row["Uses"], nil) {
				return false
			}
			am[id] = bv
		}
		for id, av := range am {
			ar, ok := av.(map[string]any)
			if !ok {
				return false
			}
			br, ok := bm[id].(map[string]any)
			if !ok {
				return false
			}
			for key, value := range br {
				switch key {
				case "Body":
					if value != "" && !reflect.DeepEqual(value, ar[key]) {
						return false
					}
					ar[key] = value
				case "Blocked":
					if ar[key] == true && value != true {
						return false
					}
					ar[key] = value
				case "Revision":
					x, xok := ar[key].(json.Number)
					y, yok := value.(json.Number)
					if !xok || !yok {
						return false
					}
					xi, xe := strconv.ParseUint(string(x), 10, 64)
					yi, ye := strconv.ParseUint(string(y), 10, 64)
					if xe != nil || ye != nil || yi < xi {
						return false
					}
					ar[key] = value
				case "Uses", "MediaRefs":
					if !subset(value, ar[key]) {
						return false
					}
					ar[key] = value
				}
			}
			if !reflect.DeepEqual(ar, br) {
				return false
			}
		}
	}
	if !subset(a["Disabled"], b["Disabled"]) {
		return false
	}
	a["Disabled"] = b["Disabled"]
	if epoch, ok := b["Epoch"].(string); !ok || domain.ValidateID(epoch) != nil {
		return false
	}
	a["Epoch"] = b["Epoch"]
	if !reflect.DeepEqual(a, b) {
		return false
	}
	return true
}

// ConfirmMemoryReplayCleanup only acknowledges already intent-blocked storage
// after the memory module's closed-permit Files purge has returned success.
func (s *Store) ConfirmMemoryReplayCleanup(ctx context.Context, r MemoryReplayWrite) error {
	binding, release, e := r.Permit.pin()
	if e != nil {
		return ErrDenied
	}
	defer release()
	if binding.Installation != r.Installation || binding.Generation != r.Generation {
		return ErrDenied
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var installation string
	var generation uint64
	if e = validateReplayInstallation(ctx, tx, binding, &installation, &generation); e != nil || generation != r.Generation {
		return ErrDenied
	}
	var hash string
	if e = tx.QueryRowContext(ctx, "SELECT ledger_hash FROM memory_replay WHERE installation_id=? AND ledger_sequence=?", r.Installation, r.Sequence).Scan(&hash); e != nil || hash != r.Hash {
		return ErrDenied
	}
	for _, asset := range r.Assets {
		if !asset.Delete {
			continue
		}
		if _, e = tx.ExecContext(ctx, "UPDATE memory_asset_lineage SET purged=1 WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=? AND storage_key=? AND delete_intent=1", r.Scope.TenantID, r.Scope.ProductID, r.Scope.WorldInstanceID, r.Subject, asset.Storage); e != nil {
			return e
		}
	}
	return s.finish(ctx, tx, BarrierRequest{commandOnly: true})
}

type MemoryReplayPermit struct {
	handle  *localstate.ExclusiveAdmission
	binding localstate.ClosedBinding
}

func IssueMemoryReplayPermit(h *localstate.ExclusiveAdmission) (*MemoryReplayPermit, error) {
	b, done, e := h.Pin()
	if e != nil {
		return nil, ErrDenied
	}
	done()
	return &MemoryReplayPermit{h, b}, nil
}
func (p *MemoryReplayPermit) pin() (localstate.ClosedBinding, func(), error) {
	if p == nil {
		return localstate.ClosedBinding{}, nil, ErrDenied
	}
	b, done, e := p.handle.Pin()
	if e != nil {
		return b, nil, ErrDenied
	}
	if b != p.binding {
		done()
		return b, nil, ErrDenied
	}
	return b, done, nil
}
func validateReplayInstallation(ctx context.Context, tx *sql.Tx, b localstate.ClosedBinding, id *string, generation *uint64) error {
	var db, files, root string
	if e := tx.QueryRowContext(ctx, "SELECT installation_id,generation,database_binding,files_binding,coordinator_binding FROM memory_installation WHERE singleton=1 FOR UPDATE").Scan(id, generation, &db, &files, &root); e != nil || *id != b.Installation || *generation > b.Generation || db != b.Database || files != b.Files || root != b.RootHash {
		return ErrDenied
	}
	return nil
}

type MemoryCandidateBinding struct {
	Installation, Ledger, Anchor, HeadHash, CandidateID, CommandKey string
	Generation, Sequence                                            uint64
}
type MemoryCandidateReceipt struct{ binding MemoryCandidateBinding }

func (r *MemoryCandidateReceipt) Matches(b MemoryCandidateBinding) bool {
	return r != nil && r.binding == b
}

// BindMemoryGovernanceCandidate is a fixed CAS. A returned private receipt
// proves a committed original binding; COMMIT_UNKNOWN cannot manufacture it.
func (s *Store) BindMemoryGovernanceCandidate(ctx context.Context, p *MemoryReplayPermit, b MemoryCandidateBinding) (*MemoryCandidateReceipt, error) {
	identity, release, e := p.pin()
	if e != nil {
		return nil, e
	}
	defer release()
	if b.Installation != identity.Installation || b.Generation != identity.Generation || b.CommandKey != identity.Plan || len(b.CandidateID) != 32 || domain.ValidateID(b.Ledger) != nil || domain.ValidateID(b.Anchor) != nil || len(b.HeadHash) != 64 {
		return nil, ErrDenied
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var installation string
	var generation uint64
	if e = validateReplayInstallation(ctx, tx, identity, &installation, &generation); e != nil {
		return nil, e
	}
	old := MemoryCandidateBinding{Installation: b.Installation, Generation: b.Generation}
	e = tx.QueryRowContext(ctx, "SELECT candidate_id,command_key,ledger_id,anchor_id,ledger_sequence,ledger_hash FROM memory_open_candidates WHERE installation_id=? AND generation=?", b.Installation, b.Generation).Scan(&old.CandidateID, &old.CommandKey, &old.Ledger, &old.Anchor, &old.Sequence, &old.HeadHash)
	if e == nil {
		if old != b {
			return nil, ErrConflict
		}
		if e = tx.Rollback(); e != nil {
			return nil, e
		}
		return &MemoryCandidateReceipt{old}, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO memory_open_candidates VALUES(?,?,?,?,?,?,?,?)", b.Installation, b.Generation, b.CandidateID, b.CommandKey, b.Ledger, b.Anchor, b.Sequence, b.HeadHash); e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE memory_installation SET generation=? WHERE singleton=1", b.Generation); e != nil {
		return nil, e
	}
	if e = s.finish(ctx, tx, BarrierRequest{commandOnly: true}); e != nil {
		return nil, e
	}
	return &MemoryCandidateReceipt{b}, nil
}
