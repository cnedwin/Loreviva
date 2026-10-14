package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strings"
	"time"
	"unicode/utf8"
)

type MemoryGovernanceCommand struct {
	Key, Action, Replacement, PolicyAction string
	PolicyRevision                         uint64
	Selector                               Selector
	Uses                                   []Use
}
type GovernanceReceipt struct{ TaskID, Blocking, Preservation, Cleanup string }

func (s *MemoryService) governanceBarrier(ctx context.Context, n Namespace) (store.CommandBarrierRequest, error) {
	g, e := s.currentGate(ctx, n.ProductScope, true)
	if e != nil {
		return store.CommandBarrierRequest{}, e
	}
	var v domain.Versions
	var state string
	e = s.Store.DB.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", n.TenantID, n.ProductID, n.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &state)
	if e != nil {
		return store.CommandBarrierRequest{}, e
	}
	return store.CommandBarrierRequest{ProductScope: n.ProductScope, WorldScope: &n.Scope, Versions: &v, OwnerSubjectID: n.SubjectID, Purpose: store.GovernancePurpose, ExpectedWorldStatus: state, GateVersions: g, Locks: n.Locks()}, nil
}
func (s *MemoryService) Govern(ctx context.Context, scope domain.RelationshipScope, c MemoryGovernanceCommand) (GovernanceReceipt, error) {
	n := Namespace{Scope: scope.Scope, DataSubjectID: scope.PlayerID}
	out := GovernanceReceipt{}
	ctx, release, permitError := s.Store.EnterOperation(ctx)
	if permitError != nil {
		return out, permitError
	}
	defer release()
	if scope.Validate() != nil || n.Validate() != nil || !validGovernanceShape(c) || domain.ValidateIdempotencyKey(c.Key) != nil || s.Ledger == nil {
		return out, ErrInvalid
	}
	intake, finish, e := s.Ledger.beginIntake(ctx)
	if e != nil {
		return out, e
	}
	defer finish()
	id := content.Identity(n.TenantID, n.ProductID, n.SubjectID, n.WorldInstanceID, "MEMORY_GOVERNANCE", c.Key)
	out.TaskID = id
	pending := &PreservationPending{TaskID: id, InstanceID: n.WorldInstanceID, ProductID: n.ProductID, DataSubjectID: n.DataSubjectID, ActorID: scope.ActorInstanceID}
	raw := governancePayload(n, scope.ActorInstanceID, c)
	hash := sha256.Sum256(raw)
	br, e := s.governanceBarrier(ctx, n)
	if e != nil {
		return out, e
	}
	rights := []string{"memory.govern"}
	if c.Selector.TargetKind == "SOURCE" {
		rights = append(rights, "memory.source.govern")
	}
	if c.Action == "CORRECT" {
		rights = append(rights, "memory.correct")
	}
	if c.Action == "SET_USE_POLICY" {
		rights = append(rights, "memory.policy")
	}
	for _, right := range rights {
		if _, _, e = s.AdminRead(ctx, n, scope.ActorInstanceID, right); e != nil {
			return out, e
		}
	}
	assetKeys, assetIDs, e := s.assetLocks(ctx, n)
	if e != nil {
		return out, e
	}
	fresh, e := s.Ledger.reserve(ctx, Reservation{ID: id, RequestHash: hex.EncodeToString(hash[:]), Scope: []string{n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, n.SubjectID}})
	if e != nil {
		return out, e
	}
	br.Locks = append(br.Locks, assetKeys...)
	br.Operation = "MEMORY_GOVERNANCE_ACCEPT"
	br.CommandKey = c.Key
	br.CommitKey = content.Identity(id, "accept")
	br.PayloadHash = hash
	br.Locks = append(br.Locks, store.LockKey{Rank: 50, IDs: []string{n.TenantID, n.ProductID, id}})
	// Apply the selected revisions or policy revision inside the namespace transaction; REVOKE removes only the selected uses when provided, while blocking and derived-asset intent stay bound to the same governance task.
	_, e = s.Store.WithCommandBarrier(ctx, br, func(tx store.Tx) error {
		if e := authorizeUserTx(ctx, tx, n, "memory.govern"); e != nil {
			return e
		}
		if c.Selector.TargetKind == "SOURCE" {
			if e := authorizeUserTx(ctx, tx, n, "memory.source.govern"); e != nil {
				return e
			}
		}
		if c.Action == "CORRECT" {
			if e := authorizeUserTx(ctx, tx, n, "memory.correct"); e != nil {
				return e
			}
		}
		a, e := loadTx(ctx, tx, n)
		if e != nil {
			return e
		}
		replacementID := ""
		if c.Action == "CORRECT" {
			replacementID = content.Identity(id, "replacement")
		}
		var binding Binding
		if c.Action == "SET_USE_POLICY" {
			if e = authorizeUserTx(ctx, tx, n, "memory.policy"); e != nil {
				return e
			}
			if e = a.SetUsePolicy(c.PolicyRevision, c.PolicyAction, c.Uses); e != nil {
				return e
			}
			binding = Binding{SelectorHash: digest(c), SourceEpoch: a.Epoch, Cutoff: a.Intake, Action: c.Action, TargetKind: c.PolicyAction, Uses: c.Uses}
		} else {
			binding, e = a.Block(c.Selector, c.Action, c.Uses, replacementID, c.Replacement)
			if e != nil {
				return e
			}
		}
		completion, _ := json.Marshal(LedgerEntry{Sequence: ^uint64(0), PrevHash: strings.Repeat("0", 64), Hash: strings.Repeat("0", 64), Kind: "APPENDED", Reservation: Reservation{ID: id, RequestHash: hex.EncodeToString(hash[:]), Scope: []string{n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, n.SubjectID}}, Binding: &binding})
		if len(completion)+1 > CompletionAllowance {
			return ErrInvalid
		}
		if e = tx.CreateRegisteredTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: n.ProductScope, WorldScope: &n.Scope, OwnerSubjectID: n.SubjectID}, TaskID: id, NextRunAt: time.Now().UTC(), Kind: "GOVERNANCE"}, "MEMORY_GOVERNANCE"); e != nil {
			return e
		}
		if e = tx.AdvanceMemoryPrivacy(ctx, n.DataSubjectID); e != nil {
			return e
		}
		if e = s.blockAssets(ctx, tx, n, a, assetIDs); e != nil {
			return e
		}
		if e = saveTx(ctx, tx, n, a); e != nil {
			return e
		}
		b, _ := json.Marshal(binding)
		if e = tx.RequireLock(n.Key()); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO memory_governance(tenant_id,product_id,task_id,world_instance_id,data_subject_id,owner_subject_id,request_hash,binding,preservation,cleanup) VALUES(?,?,?,?,?,?,?,?,'PENDING','PENDING')", n.TenantID, n.ProductID, id, n.WorldInstanceID, n.DataSubjectID, n.SubjectID, hash[:], b)
		return e
	})
	// Keep an uncertain blocking outcome pending under its original task; seal rejection only with the Store's rejection proof, and append preservation after the DB unit of work ends.
	if e != nil {
		if errors.Is(e, store.ErrCommitUnknown) {
			return out, pending
		}
		if !fresh {
			return out, pending
		}
		proof, proofError := s.Store.ProveMemoryRejection(ctx, e, id)
		if proofError != nil {
			return out, pending
		}
		if sealError := s.Ledger.SealRejectedOnce(ctx, intake, id, proof); sealError != nil {
			return out, pending
		}
		return out, e
	}
	// G2 performs file I/O only after the DB UoW has finished.
	out.Blocking = "BLOCKED"
	var b []byte
	if e = s.Store.DB.QueryRowContext(ctx, "SELECT binding FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", n.TenantID, n.ProductID, id, n.SubjectID).Scan(&b); e != nil {
		return out, pending
	}
	var binding Binding
	if json.Unmarshal(b, &binding) != nil {
		return out, pending
	}
	if e = s.Ledger.AppendOnce(ctx, id, binding); e != nil {
		return out, pending
	}
	lease, e := s.Store.ClaimRegistered(ctx, "memory-preserver", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "GOVERNANCE", Operation: "MEMORY_GOVERNANCE", TaskID: id}})
	if e != nil {
		if errors.Is(e, store.ErrNoTask) {
			status, statusError := s.Status(ctx, n, id)
			if statusError != nil {
				return out, pending
			}
			if status.Preservation != "CONFIRMED" {
				return status, pending
			}
			return status, nil
		}
		return out, pending
	}
	if e = s.Process(ctx, lease, "memory-preserver"); e != nil {
		return out, pending
	}
	status, statusError := s.Status(ctx, n, id)
	if statusError != nil {
		return out, pending
	}
	if status.Preservation != "CONFIRMED" {
		return status, pending
	}
	return status, nil
}

// Process resumes exactly the existing task. It does not create a new command,
// source, task framework or restoration authority.
// Resume only the registered PROGRAM-owned cleanup and its original accepted intent; preserved ledger evidence and confirmed purges are required before marking local cleanup complete.
func (s *MemoryService) Process(ctx context.Context, lease store.RegisteredLease, worker string) error {
	ctx, release, permitError := s.Store.EnterOperation(ctx)
	if permitError != nil {
		return permitError
	}
	defer release()
	if lease.Kind != "GOVERNANCE" || lease.Operation != "MEMORY_GOVERNANCE" || lease.WorldInstanceID == nil || s.Ledger == nil {
		return store.ErrDenied
	}
	if e := s.Store.CheckProgramCleanup(ctx, lease, worker); e != nil {
		return e
	}
	n := Namespace{Scope: domain.Scope{ProductScope: lease.ProductScope, WorldInstanceID: *lease.WorldInstanceID}}
	var raw []byte
	if e := s.Store.DB.QueryRowContext(ctx, "SELECT data_subject_id,binding FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=?", n.TenantID, n.ProductID, lease.TaskID, n.SubjectID).Scan(&n.DataSubjectID, &raw); e != nil {
		return e
	}
	var binding Binding
	if json.Unmarshal(raw, &binding) != nil {
		return ErrInvalid
	}
	if e := s.Ledger.AppendOnce(ctx, lease.TaskID, binding); e != nil {
		return e
	}
	entry, e := s.Ledger.Lookup(ctx, lease.TaskID)
	if e != nil || entry.Kind != "APPENDED" || digest(entry.Binding) != digest(binding) {
		return ErrPreservation
	}
	assetKeys, assetIDs, e := s.assetLocks(ctx, n)
	if e != nil {
		return e
	}
	confirmed, e := s.purgeAssets(ctx, n, assetIDs)
	if e != nil {
		return e
	}
	br, e := s.governanceBarrier(ctx, n)
	if e != nil {
		return e
	}
	br.Locks = append(br.Locks, assetKeys...)
	_, e = s.Store.WithBarrier(ctx, store.BarrierRequest{ProductScope: br.ProductScope, WorldScope: br.WorldScope, Versions: br.Versions, OwnerSubjectID: n.SubjectID, Purpose: store.GovernancePurpose, ExpectedWorldStatus: br.ExpectedWorldStatus, GateVersions: br.GateVersions, Locks: br.Locks, TaskID: lease.TaskID, WorkerID: worker, Fence: lease.Fence, Operation: "MEMORY_GOVERNANCE", CommandKey: lease.TaskID, CommitKey: content.Identity(lease.TaskID, "preserved-clean"), PayloadHash: sha256.Sum256(raw)}, func(tx store.Tx) error {
		a, e := loadTx(ctx, tx, n)
		if e != nil {
			return e
		}
		if binding.ReplacementID != "" {
			replacement, ok := a.Nodes[binding.ReplacementID]
			if !ok || digest(replacement.Body) != binding.ReplacementHash {
				if e = tx.RequireLock(n.Key()); e != nil {
					return e
				}
				if _, e = tx.ExecContext(ctx, "UPDATE memory_governance SET preservation='BLOCKED_NEEDS_REPLACEMENT',cleanup='PENDING' WHERE tenant_id=? AND product_id=? AND task_id=?", n.TenantID, n.ProductID, lease.TaskID); e != nil {
					return e
				}
				return tx.SetTaskState(ctx, "WAITING_EXTERNAL")
			}
		}
		if e = acknowledgePurgedAssets(ctx, tx, n, assetIDs, confirmed); e != nil {
			return e
		}
		a.Clean()
		if e = saveTx(ctx, tx, n, a); e != nil {
			return e
		}
		if e = tx.RequireLock(n.Key()); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE memory_governance SET preservation='CONFIRMED',cleanup='LOCAL_COMPLETE' WHERE tenant_id=? AND product_id=? AND task_id=?", n.TenantID, n.ProductID, lease.TaskID); e != nil {
			return e
		}
		return tx.SetTaskState(ctx, "COMMITTED")
	})
	return e
}
func (s *MemoryService) Status(ctx context.Context, n Namespace, id string) (GovernanceReceipt, error) {
	out := GovernanceReceipt{TaskID: id, Blocking: "BLOCKED"}
	ctx, release, e := s.Store.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	if n.Validate() != nil || domain.ValidateID(id) != nil || s.Ledger == nil {
		return out, store.ErrDenied
	}
	if _, e := s.currentGate(ctx, n.ProductScope, true); e != nil {
		return out, e
	}
	read, e := s.Store.BeginMemoryRead(ctx, n.ProductScope, "memory.govern", n.DataSubjectID)
	if e != nil {
		return out, e
	}
	defer read.Close()
	var owner bool
	if e := read.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM runtime_instances WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND owner_subject_id=?)", n.TenantID, n.ProductID, n.WorldInstanceID, n.SubjectID).Scan(&owner); e != nil || !owner {
		return out, store.ErrDenied
	}
	var permitted bool
	if e := read.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM memory_subject_permissions p JOIN subject_governance g ON g.tenant_id=p.tenant_id AND g.product_id=p.product_id AND g.subject_id=p.data_subject_id WHERE p.tenant_id=? AND p.product_id=? AND p.caller_subject_id=? AND p.data_subject_id=? AND p.action='memory.govern' AND p.allowed_use='PRIVATE_RESPONSE' AND p.enabled=1 AND g.enabled=1 AND p.expires_at>UTC_TIMESTAMP(6))", n.TenantID, n.ProductID, n.SubjectID, n.DataSubjectID).Scan(&permitted); e != nil || !permitted {
		return out, store.ErrDenied
	}
	e = read.QueryRowContext(ctx, "SELECT preservation,cleanup FROM memory_governance WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND data_subject_id=? AND owner_subject_id=? AND task_id=?", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, n.SubjectID, id).Scan(&out.Preservation, &out.Cleanup)
	if finishError := read.Finish(ctx); finishError != nil {
		return out, finishError
	}
	if errors.Is(e, sql.ErrNoRows) {
		entry, x := s.Ledger.Lookup(ctx, id)
		if x != nil || digest(entry.Reservation.Scope) != digest([]string{n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, n.SubjectID}) {
			return out, store.ErrDenied
		}
		if entry.Kind == "REJECTED" {
			return GovernanceReceipt{TaskID: id, Blocking: "NOT_APPLIED", Preservation: "CONFIRMED", Cleanup: "NOT_REQUIRED"}, nil
		}
		return GovernanceReceipt{TaskID: id, Blocking: "UNKNOWN", Preservation: "PENDING", Cleanup: "PENDING"}, nil
	}
	if e != nil {
		return out, e
	}
	return out, nil
}

func (s *MemoryService) AdminRead(ctx context.Context, n Namespace, actor, action string) (Aggregate, uint64, error) {
	var empty Aggregate
	ctx, release, permitError := s.Store.EnterOperation(ctx)
	if permitError != nil {
		return empty, 0, permitError
	}
	defer release()
	if n.Validate() != nil || domain.ValidateID(actor) != nil {
		return empty, 0, store.ErrDenied
	}
	if _, e := s.currentGate(ctx, n.ProductScope, false); e != nil {
		return empty, 0, e
	}
	read, e := s.Store.BeginMemoryRead(ctx, n.ProductScope, action, n.DataSubjectID)
	if e != nil {
		return empty, 0, e
	}
	defer read.Close()
	var b []byte
	var epoch uint64
	e = read.QueryRowContext(ctx, "SELECT m.document,w.privacy_epoch FROM memory_aggregates m JOIN runtime_instances i ON i.tenant_id=m.tenant_id AND i.product_id=m.product_id AND i.world_instance_id=m.world_instance_id JOIN world_controls w ON w.tenant_id=i.tenant_id AND w.product_id=i.product_id AND w.world_instance_id=i.world_instance_id JOIN releases r ON r.tenant_id=w.tenant_id AND r.product_id=w.product_id AND r.release_id=w.release_id JOIN subject_governance g ON g.tenant_id=m.tenant_id AND g.product_id=m.product_id AND g.subject_id=m.data_subject_id WHERE m.tenant_id=? AND m.product_id=? AND m.world_instance_id=? AND m.data_subject_id=? AND i.owner_subject_id=? AND r.available=1 AND g.enabled=1 AND EXISTS(SELECT 1 FROM actors a WHERE a.tenant_id=m.tenant_id AND a.product_id=m.product_id AND a.world_instance_id=m.world_instance_id AND a.actor_id=?) AND EXISTS(SELECT 1 FROM memory_subject_permissions p WHERE p.tenant_id=m.tenant_id AND p.product_id=m.product_id AND p.caller_subject_id=? AND p.data_subject_id=m.data_subject_id AND p.action=? AND p.enabled=1 AND p.expires_at>UTC_TIMESTAMP(6) AND p.allowed_use='PRIVATE_RESPONSE')", n.TenantID, n.ProductID, n.WorldInstanceID, n.DataSubjectID, n.SubjectID, actor, n.SubjectID, action).Scan(&b, &epoch)
	if e != nil {
		return empty, 0, store.ErrDenied
	}
	if e = read.Finish(ctx); e != nil {
		return empty, 0, e
	}
	e = json.Unmarshal(b, &empty)
	return empty, epoch, e
}

type PreservationPending struct{ TaskID, InstanceID, ProductID, DataSubjectID, ActorID string }

func (e *PreservationPending) Error() string {
	return "governance blocking outcome requires original task lookup"
}
func (e *PreservationPending) Unwrap() error { return ErrPreservation }

func (s *MemoryService) currentGate(ctx context.Context, scope domain.ProductScope, govern bool) (store.GateVersions, error) {
	var g store.GateVersions
	if scope.Validate() != nil {
		return g, store.ErrDenied
	}
	var enabled, canGovern bool
	e := s.Store.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,(t.enabled AND p.enabled AND s.enabled),s.can_govern FROM subject_governance s JOIN product_gates p ON p.tenant_id=s.tenant_id AND p.product_id=s.product_id JOIN tenant_gates t ON t.tenant_id=s.tenant_id WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &enabled, &canGovern)
	if e != nil || !enabled || govern && !canGovern {
		return g, store.ErrDenied
	}
	return g, nil
}

func governancePayload(n Namespace, actor string, c MemoryGovernanceCommand) []byte {
	raw, _ := json.Marshal(struct {
		Namespace Namespace
		ActorID   string
		Command   MemoryGovernanceCommand
	}{n, actor, c})
	return raw
}
func validGovernanceShape(c MemoryGovernanceCommand) bool {
	if !utf8.ValidString(c.Replacement) || utf8.RuneCountInString(c.Replacement) > 16000 || len(c.Selector.IDs) > 1000 || len(c.Selector.Expected) > 1000 || c.Uses != nil && !validUses(c.Uses) {
		return false
	}
	if c.Action == "SET_USE_POLICY" {
		return c.Replacement == "" && c.PolicyRevision > 0 && (c.PolicyAction == "DISABLE" || c.PolicyAction == "ENABLE") && validUses(c.Uses)
	}
	if c.Action != "DELETE" && c.Action != "REVOKE" && c.Action != "CORRECT" {
		return false
	}
	if c.Action != "CORRECT" && c.Replacement != "" {
		return false
	}
	if c.Action == "CORRECT" && (c.Selector.Kind != "IDS" || c.Selector.TargetKind != "MEMORY_NODE" || len(c.Selector.IDs) != 1 || c.Replacement == "") {
		return false
	}
	switch c.Selector.Kind {
	case "IDS":
		if len(c.Selector.IDs) == 0 || len(c.Selector.IDs) != len(c.Selector.Expected) {
			return false
		}
		for _, id := range c.Selector.IDs {
			if domain.ValidateID(id) != nil || c.Selector.Expected[id] == 0 {
				return false
			}
		}
		return c.Selector.TargetKind == "MEMORY_NODE" || c.Selector.TargetKind == "SOURCE"
	case "ALL_EXISTING":
		return c.Selector.TargetKind == "SOURCE" && len(c.Selector.IDs) == 0 && len(c.Selector.Expected) == 0
	case "TIME_RANGE":
		return c.Selector.TargetKind == "SOURCE" && len(c.Selector.IDs) == 0 && len(c.Selector.Expected) == 0 && !c.Selector.From.IsZero() && c.Selector.From.Before(c.Selector.To) && c.Selector.From.Location() == time.UTC && c.Selector.To.Location() == time.UTC
	}
	return false
}
func (s *MemoryService) MatchesOriginalGovernance(ctx context.Context, scope domain.RelationshipScope, c MemoryGovernanceCommand) bool {
	if s.Ledger == nil || scope.Validate() != nil || !validGovernanceShape(c) {
		return false
	}
	n := Namespace{Scope: scope.Scope, DataSubjectID: scope.PlayerID}
	id := content.Identity(n.TenantID, n.ProductID, n.SubjectID, n.WorldInstanceID, "MEMORY_GOVERNANCE", c.Key)
	entry, e := s.Ledger.Lookup(ctx, id)
	if e != nil || entry.Kind == "REJECTED" {
		return false
	}
	hash := sha256.Sum256(governancePayload(n, scope.ActorInstanceID, c))
	return entry.Reservation.RequestHash == hex.EncodeToString(hash[:])
}
