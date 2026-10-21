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
	world "loreviva.local/loreviva/internal/runtime"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"unicode/utf8"
)

func (m *MemoryService) PlanGenerationLocks(ctx context.Context, s domain.Scope, actor string) ([]store.LockKey, error) {
	keys, e := world.PerceptionLocks(ctx, m.Store, s, actor)
	if e != nil {
		return nil, e
	}
	var release string
	if e = m.Store.DB.QueryRowContext(ctx, "SELECT release_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", s.TenantID, s.ProductID, s.WorldInstanceID).Scan(&release); e != nil {
		return nil, e
	}
	keys = append(keys, content.Key(s.ProductScope, "content_release", release))
	return append(keys, GenerationContextLocks(ActorGenerationRequest{Scope: s, ActorID: actor, DataSubjectID: s.SubjectID, TargetKind: "PLAYER", TargetID: s.SubjectID})...), nil
}

func (m *MemoryService) FreezeHarnessBindingTx(ctx context.Context, tx store.Tx, s domain.Scope, task string, selection domain.GenerationSelection) (domain.GenerationBinding, error) {
	return m.FreezeHarnessContextTx(ctx, tx, ActorGenerationRequest{Scope: s, TaskID: task, ActorID: selection.ActorID, DataSubjectID: selection.DataSubjectID, TargetKind: selection.TargetKind, TargetID: selection.TargetID, Audience: Private, Use: PrivateResponse})
}

func (m *MemoryService) FreezeHarnessContextTx(ctx context.Context, tx store.Tx, r ActorGenerationRequest) (domain.GenerationBinding, error) {
	b, e := currentHarnessBinding(ctx, tx, r)
	if e != nil {
		return b, e
	}
	if e = tx.BindHarnessContext(ctx, r.Scope, r.TaskID, b); e != nil {
		return domain.GenerationBinding{}, e
	}
	return b, nil
}
func (m *MemoryService) ValidateHarnessContextTx(ctx context.Context, tx store.Tx, s domain.Scope, task string) (domain.GenerationBinding, error) {
	if e := tx.AssertWorldTask(s, task); e != nil {
		return domain.GenerationBinding{}, e
	}
	var raw []byte
	if e := tx.QueryRowContext(ctx, "SELECT document FROM harness_context_bindings WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&raw); e != nil {
		return domain.GenerationBinding{}, e
	}
	var frozen domain.GenerationBinding
	if e := json.Unmarshal(raw, &frozen); e != nil {
		return domain.GenerationBinding{}, e
	}
	r := ActorGenerationRequest{Scope: s, TaskID: task, ActorID: frozen.ActorID, DataSubjectID: frozen.DataSubjectID, TargetKind: frozen.TargetKind, TargetID: frozen.TargetID, Audience: Audience(frozen.Audience), Use: Use(frozen.Use)}
	current, e := currentHarnessBinding(ctx, tx, r)
	if e != nil {
		return domain.GenerationBinding{}, e
	}
	if current != frozen {
		return domain.GenerationBinding{}, store.ErrConflict
	}
	return current, nil
}

func currentHarnessBinding(ctx context.Context, tx store.Tx, r ActorGenerationRequest) (domain.GenerationBinding, error) {
	m, e := currentGenerationMaterial(ctx, tx, r)
	return m.Binding, e
}

func (m *MemoryService) BuildGenerationRequestTx(ctx context.Context, tx store.Tx, s domain.Scope, task string) (domain.GenerationMaterial, error) {
	b, e := m.ValidateHarnessContextTx(ctx, tx, s, task)
	if e != nil {
		return domain.GenerationMaterial{}, e
	}
	return currentGenerationMaterial(ctx, tx, ActorGenerationRequest{Scope: s, TaskID: task, ActorID: b.ActorID, DataSubjectID: b.DataSubjectID, TargetKind: b.TargetKind, TargetID: b.TargetID, Audience: Audience(b.Audience), Use: Use(b.Use)})
}

func currentGenerationMaterial(ctx context.Context, tx store.Tx, r ActorGenerationRequest) (out domain.GenerationMaterial, err error) {
	return currentGenerationMaterialAtOriginalRevision(ctx, tx, r, nil, nil, nil)
}

func currentGenerationMaterialAtOriginalRevision(ctx context.Context, tx store.Tx, r ActorGenerationRequest, original *domain.Versions, originalWorld *domain.WorldPerception, keep map[string]string) (out domain.GenerationMaterial, err error) {
	c, e := assembleGenerationContextTx(ctx, tx, r, original)
	if e != nil {
		return out, e
	}
	if len(keep) != 0 {
		// An original attempt's proof covers the memories it actually used. A
		// memory committed by another task in the same world is not part of that
		// set, so it must not invalidate the attempt's own proof.
		c.Initial = originalNodesOnly(c.Initial, keep)
		c.Relationship = originalNodesOnly(c.Relationship, keep)
	}
	route, e := world.GenerationRouteTx(ctx, tx, r.Scope, r.ActorID)
	if e != nil {
		return out, e
	}
	routeHash, e := content.Hash(route)
	if e != nil {
		return out, e
	}
	turn, e := currentTurnEvidence(ctx, tx, r, c)
	if e != nil {
		return out, e
	}
	perception, e := world.PerceptionTx(ctx, tx, r.Scope, r.ActorID)
	if e != nil {
		return out, e
	}
	if originalWorld != nil {
		if !world.OriginalPerceptionStillAllowed(*originalWorld, perception) {
			return out, store.ErrDenied
		}
		perception = *originalWorld
	}
	worldHash, e := content.Hash(perception)
	if e != nil {
		return out, e
	}
	skills, e := world.TurnSkillInputsTx(ctx, tx, r.Scope, r.ActorID)
	if e != nil {
		return out, e
	}
	preimage := struct {
		Core                  ActorCore
		Initial, Relationship []MemoryRecord
		Route                 world.GenerationRoute
		Turn                  *turnEvidence
		World                 domain.WorldPerception
		Skills                []domain.TurnSkillInput `json:",omitempty"`
	}{c.Core, c.Initial, c.Relationship, route, turn, perception, skills}
	raw, e := json.Marshal(preimage)
	if e != nil {
		return out, e
	}
	canonical, e := wire.Canonical(raw)
	if e != nil {
		return out, e
	}
	if turn != nil && uint64(utf8.RuneCount(canonical)) > turn.InputBound {
		return out, store.ErrDenied
	}
	requestHash, e := content.Hash(preimage)
	if e != nil {
		return out, e
	}
	b := domain.GenerationBinding{ActorID: r.ActorID, DataSubjectID: r.DataSubjectID, TargetKind: r.TargetKind, TargetID: r.TargetID, Audience: string(r.Audience), Use: string(r.Use), ContextHash: c.Proof.hash, RouteID: routeHash, RequestHash: requestHash}
	b.WorldHash = worldHash
	b.CoreHash = digest(c.Core)
	b.SkillsHash = digest(skills)
	b.MemoryGraphHash = digest(struct{ Initial, Relationship []MemoryRecord }{c.Initial, c.Relationship})
	if turn != nil {
		b.InputSourceID = turn.SourceID
		b.TurnCommandHash = turn.CommandHash
	}
	if turn == nil {
		out.Binding = b
		return out, nil
	}
	out = domain.GenerationMaterial{Binding: b, Canonical: canonical, Connection: route.Connection, Model: route.Model, Module: route.Module, InputBound: turn.InputBound, OutputBound: turn.OutputBound}
	if route.Settings.Parameters != nil && route.Settings.Parameters.MaxOutputTokens != nil {
		limit := *route.Settings.Parameters.MaxOutputTokens
		if limit <= 0 {
			return domain.GenerationMaterial{}, store.ErrDenied
		}
		if uint64(limit) < out.OutputBound {
			out.OutputBound = uint64(limit)
		}
	}
	return out, nil
}

type turnEvidence struct {
	CommandHash, QuoteID, SourceID, InputHash string
	InputBound, OutputBound                   uint64
}

// originalNodesOnly keeps the records an original attempt actually used. The
// kept records are hashed in full, so a modified body still changes the digest
// and is still refused; only unrelated additions are ignored.
func originalNodesOnly(in []MemoryRecord, keep map[string]string) []MemoryRecord {
	out := make([]MemoryRecord, 0, len(in))
	for _, n := range in {
		if _, ok := keep[n.ID]; ok {
			out = append(out, n)
		}
	}
	return out
}

func currentTurnEvidence(ctx context.Context, tx store.Tx, r ActorGenerationRequest, c ActorGenerationContext) (*turnEvidence, error) {
	var actor string
	var command, input []byte
	out := &turnEvidence{}
	e := tx.QueryRowContext(ctx, "SELECT h.actor_id,h.command_hash,h.quote_id,h.input_source_id,h.input_hash,q.input_bound,q.output_bound FROM harness_turns h JOIN harness_turn_quotes q ON q.tenant_id=h.tenant_id AND q.product_id=h.product_id AND q.quote_id=h.quote_id WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=?", r.Scope.TenantID, r.Scope.ProductID, r.TaskID, r.Scope.WorldInstanceID, r.Scope.SubjectID).Scan(&actor, &command, &out.QuoteID, &out.SourceID, &input, &out.InputBound, &out.OutputBound)
	if errors.Is(e, sql.ErrNoRows) {
		var managed bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_envelopes e JOIN harness_turn_quotes q ON q.tenant_id=e.tenant_id AND q.product_id=e.product_id AND q.quote_id=e.quote_id WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?)", r.Scope.TenantID, r.Scope.ProductID, r.TaskID).Scan(&managed); e != nil {
			return nil, e
		}
		if managed {
			return nil, store.ErrDenied
		}
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if actor != r.ActorID || r.DataSubjectID != r.Scope.SubjectID || r.TargetKind != "PLAYER" || r.TargetID != r.Scope.SubjectID || len(command) != 32 || len(input) != 32 {
		return nil, store.ErrDenied
	}
	graph, e := loadTx(ctx, tx, Namespace{Scope: r.Scope, DataSubjectID: r.Scope.SubjectID})
	if e != nil {
		return nil, e
	}
	source, ok := graph.Sources[out.SourceID]
	basis, e := tx.RehearsalBasis(r.Scope)
	if e != nil {
		return nil, e
	}
	validSource := source.Body != "" && source.Domain == PlayerStatement
	if basis != nil {
		validSource = source.Domain == Sandbox && basis.ActorID == actor
	}
	if !ok || !validSource || source.Audience != Private {
		return nil, store.ErrDenied
	}
	h := sha256.Sum256([]byte(source.Body))
	if hex.EncodeToString(h[:]) != hex.EncodeToString(input) {
		return nil, store.ErrConflict
	}
	told := false
	for _, a := range graph.Acquisitions {
		if a.ActorID == actor && a.SourceID == out.SourceID && a.Kind == Told && a.EvidenceID == r.TaskID {
			told = true
		}
	}
	eligible := false
	for _, n := range c.Relationship {
		if n.ID == content.Identity(out.SourceID, actor, "turn-input") && len(n.Sources) == 1 && n.Sources[0] == out.SourceID && n.Body == source.Body {
			eligible = true
		}
	}
	if !told || !eligible {
		return nil, store.ErrDenied
	}
	out.CommandHash = hex.EncodeToString(command)
	out.InputHash = hex.EncodeToString(input)
	return out, nil
}
