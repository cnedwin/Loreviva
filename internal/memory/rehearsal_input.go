package memory

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

// Fixed post-Apply proof: new sandbox nodes may exist, but every selected
// original source, acquisition, grant and result lineage is still current.
// This does not authorize a new request or substitute a historic reader.
func (m *MemoryService) ValidateRehearsalAppliedSourcesTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, c domain.CandidateCommit) error {
	b, e := tx.RehearsalBasis(s)
	if e != nil {
		return e
	}
	if b == nil {
		return store.ErrDenied
	}
	if _, e = AssembleGenerationContextTx(ctx, tx, ActorGenerationRequest{Scope: s, TaskID: task, ActorID: b.ActorID, DataSubjectID: s.SubjectID, TargetKind: "PLAYER", TargetID: s.SubjectID, Audience: Private, Use: PrivateResponse}); e != nil {
		return e
	}
	a, e := loadTx(ctx, tx, Namespace{Scope: s, DataSubjectID: s.SubjectID})
	if e != nil {
		return e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	metas := []domain.MockResultMetadata{c.Metadata}
	if c.OriginalDependency != nil {
		metas = append([]domain.MockResultMetadata{*c.OriginalDependency}, metas...)
	}
	for _, meta := range metas {
		var raw []byte
		var deleted bool
		if e = tx.QueryRowContext(ctx, "SELECT r.document,l.delete_intent FROM harness_result_sources r JOIN memory_asset_lineage l USING(tenant_id,product_id,storage_key) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.attempt_id=? AND r.storage_key=? AND l.world_instance_id=? AND l.data_subject_id=? AND l.actor_id=?", s.TenantID, s.ProductID, task, meta.Identity.AttemptID, meta.BodyStorage, s.WorldInstanceID, s.SubjectID, b.ActorID).Scan(&raw, &deleted); e != nil {
			return e
		}
		var refs domain.ResultSourceBinding
		if deleted || json.Unmarshal(raw, &refs) != nil || refs.Storage != meta.BodyStorage || refs.BodyHash != meta.BodyHash || refs.SubjectID != s.SubjectID || !resultSourcesAllowed(raw, a, b.ActorID, now) {
			return store.ErrDenied
		}
	}
	return nil
}

// No player consent or ordinary private memory authority is inferred. This
// exact synthetic input was frozen by the current server-owned root command.
func (m *MemoryService) AcceptRehearsalInputTx(ctx context.Context, tx store.Tx, in domain.TurnInput) error {
	if e := tx.AssertRehearsalInput(in); e != nil {
		return e
	}
	n := Namespace{Scope: in.Scope, DataSubjectID: in.Scope.SubjectID}
	a, e := loadTx(ctx, tx, n)
	if e != nil {
		return e
	}
	if domain.ValidateID(in.SourceID) != nil || a.Intake == ^uint64(0) {
		return ErrInvalid
	}
	if _, ok := a.Sources[in.SourceID]; ok {
		return store.ErrConflict
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	a.Intake++
	uses := []Use{PrivateResponse}
	a.Sources[in.SourceID] = Source{ID: in.SourceID, Body: in.Body, Domain: Sandbox, Audience: Private, Uses: uses, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: now}
	a.Grants = append(a.Grants, Grant{ActorID: in.ActorID, SourceID: in.SourceID, Uses: uses})
	a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: in.ActorID, SourceID: in.SourceID, Kind: Told, EvidenceID: in.TaskID})
	id := content.Identity(in.SourceID, in.ActorID, "turn-input")
	a.Nodes[id] = MemoryRecord{ID: id, Kind: "EXPERIENCE", ActorID: in.ActorID, TargetKind: "PLAYER", TargetID: in.Scope.SubjectID, Domain: Sandbox, Audience: Private, Uses: uses, Sources: []string{in.SourceID}, Body: in.Body, Revision: 1}
	return saveTx(ctx, tx, n, a)
}
