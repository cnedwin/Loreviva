package memory

import (
	"context"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"time"
)

func (m *MemoryService) ResolveTurnRouteTx(ctx context.Context, tx store.Tx, s domain.Scope, actor string) (domain.TurnRouteAuthority, error) {
	var out domain.TurnRouteAuthority
	n := Namespace{Scope: s, DataSubjectID: s.SubjectID}
	for _, k := range n.Locks() {
		if e := tx.RequireLock(k); e != nil {
			return out, e
		}
	}
	if e := authorizeTx(ctx, tx, n, "memory.generation.use"); e != nil {
		return out, e
	}
	init, e := world.InitializationEvidence(ctx, tx, s)
	if e != nil {
		return out, e
	}
	exists := false
	for _, a := range init.Actors {
		if a.ID == actor {
			exists = true
		}
	}
	if !exists {
		return out, store.ErrDenied
	}
	route, e := world.GenerationRouteTx(ctx, tx, s, actor)
	if e != nil {
		return out, e
	}
	if _, e = world.TurnSkillsTx(ctx, tx, s, actor); e != nil {
		return out, e
	}
	hash, e := content.Hash(route)
	if e != nil {
		return out, e
	}
	return domain.TurnRouteAuthority{ProjectID: init.ProjectID, ActorID: actor, RouteID: hash}, nil
}

func (m *MemoryService) AcceptTurnInputTx(ctx context.Context, tx store.Tx, in domain.TurnInput) error {
	if e := tx.AssertTurnIntake(in.Scope, in.TaskID); e != nil {
		return e
	}
	n := Namespace{Scope: in.Scope, DataSubjectID: in.Scope.SubjectID}
	if _, e := m.ResolveTurnRouteTx(ctx, tx, in.Scope, in.ActorID); e != nil {
		return e
	}
	a, e := loadTx(ctx, tx, n)
	if e != nil {
		return e
	}
	if in.Body == "" || domain.ValidateID(in.SourceID) != nil || a.Intake == ^uint64(0) {
		return ErrInvalid
	}
	if _, exists := a.Sources[in.SourceID]; exists {
		return store.ErrConflict
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	a.Intake++
	uses := []Use{PrivateResponse}
	a.Sources[in.SourceID] = Source{ID: in.SourceID, Body: in.Body, Domain: PlayerStatement, Audience: Private, Uses: uses, Revision: 1, Epoch: a.Epoch, Intake: a.Intake, OccurredAt: now}
	a.Grants = append(a.Grants, Grant{ActorID: in.ActorID, SourceID: in.SourceID, Uses: uses})
	a.Acquisitions = append(a.Acquisitions, Acquisition{ActorID: in.ActorID, SourceID: in.SourceID, Kind: Told, EvidenceID: in.TaskID})
	id := content.Identity(in.SourceID, in.ActorID, "turn-input")
	a.Nodes[id] = MemoryRecord{ID: id, Kind: "EXPERIENCE", ActorID: in.ActorID, TargetKind: "PLAYER", TargetID: in.Scope.SubjectID, Domain: PlayerStatement, Audience: Private, Uses: uses, Sources: []string{in.SourceID}, Body: in.Body, Revision: 1}
	return saveTx(ctx, tx, n, a)
}
