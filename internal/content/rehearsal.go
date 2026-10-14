package content

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
)

// Transient authoring material, not a published Release or a permission token.
// Durable owners retain immutable metadata and governed original references.
type RehearsalDefinition struct {
	RoutePolicy           *domain.RoutePolicySnapshot
	Draft                 generated.ContentDraft
	Resolved              ResolvedDefinitions
	Settings              generated.GenerationSettings
	Sources               []domain.CandidateSource
	SourceHash, RouteHash string
	ActorSlots            []string
}

// These checks use the same current source closure and resolved duty validator
// as real intake. They do not confer a permission or publish the definition.
func (s *Service) RehearsalContentAssertionsTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, basis domain.RehearsalBasis) (bool, bool, error) {
	m, e := s.BuildRehearsalDefinitionTx(ctx, tx, scope, basis.Binding.DraftID, basis.Request)
	if e != nil {
		return false, false, e
	}
	if m.SourceHash != basis.Binding.SourceHash || m.Draft.ContentHash != basis.Binding.DraftHash {
		return false, false, store.ErrConflict
	}
	findings, covered := definitionFindings(m.Draft.Input, "WORLD", false, m.Resolved)
	duties := len(findings) == 0 && len(covered) == len(Duties)
	for i, d := range covered {
		if i >= len(Duties) || d != Duties[i] {
			duties = false
		}
	}
	return len(m.Sources) > 0, duties, nil
}

func (s *Service) PlanRehearsalLocks(ctx context.Context, scope domain.ProductScope, draft string) ([]store.LockKey, error) {
	d, e := s.snapshotDraft(ctx, scope, draft)
	if e != nil {
		return nil, e
	}
	keys, e := s.candidateSourceLocks(ctx, scope, d.Input)
	if e != nil {
		return nil, e
	}
	return append(append(keys, Key(scope, "content_draft", draft)), store.RoutePolicyLocks(scope)...), nil
}

func (s *Service) InspectRehearsal(ctx context.Context, scope domain.ProductScope, draft string, request generated.RehearsalRequest) (out RehearsalDefinition, err error) {
	keys, e := s.PlanRehearsalLocks(ctx, scope, draft)
	if e != nil {
		return out, e
	}
	g, e := Gate(ctx, s.Store, scope)
	if e != nil {
		return out, e
	}
	key := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, draft, "rehearsal-inspect")
	done := errors.New("rehearsal inspected without mutation")
	_, e = s.Store.WithCommandBarrier(ctx, store.CommandBarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "REHEARSAL_INSPECT", CommandKey: key, CommitKey: key, GateVersions: g, PayloadHash: sha256.Sum256([]byte(key)), Locks: keys}, func(tx store.Tx) error {
		var x error
		out, x = s.BuildRehearsalDefinitionTx(ctx, tx, scope, draft, request)
		if x != nil {
			return x
		}
		return done
	})
	if !errors.Is(e, done) {
		return RehearsalDefinition{}, e
	}
	return out, nil
}

// Build the rehearsal binding only after checking the current draft, source graph, route and input actor slots in the owning transaction; the binding carries hashes rather than the original input text.
func (s *Service) BuildRehearsalDefinitionTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, draft string, request generated.RehearsalRequest) (out RehearsalDefinition, err error) {
	b, e := json.Marshal(request)
	if e != nil || generated.Validate("RehearsalRequest", b) != nil || string(request.ProductID) != scope.ProductID || len(request.Inputs) < 1 || len(request.Inputs) > 20 {
		return out, ErrBlocked
	}
	if _, e = domain.LimitsFor("REHEARSAL", len(request.Inputs)); e != nil {
		return out, e
	}
	if e = store.CheckOperatorTx(ctx, tx, scope, "content.rehearse"); e != nil {
		return out, e
	}
	rev, e := strconv.ParseUint(string(request.ExpectedDraftRevision), 10, 64)
	if e != nil {
		return out, e
	}
	d, e := current(ctx, tx, scope, draft, rev)
	if e != nil {
		return out, e
	}
	if d.RootKind != "WORLD" || d.ProjectID == nil || (d.State != "DRAFT" && d.State != "ISOLATED_IMPORT") {
		return out, ErrBlocked
	}
	h, e := Hash(d.Input)
	if e != nil || h != d.ContentHash {
		return out, ErrBlocked
	}
	resolved, e := ResolvePinned(ctx, tx, scope, d.Input)
	if e != nil {
		return out, e
	}
	if e = validateRehearsalSkills(ctx, tx, scope, d.Input, resolved); e != nil {
		return out, e
	}
	policy, e := store.ReadRoutePolicyTx(ctx, tx, scope)
	if e != nil {
		return out, e
	}
	findings, _ := definitionFindingsPolicy(d.Input, "WORLD", false, resolved, policy)
	if len(findings) > 0 {
		return out, &FieldErrors{findings}
	}
	sources, e := sourceGraphTx(ctx, tx, scope, d.Input, string(d.Input.RootObjectID), true)
	if e != nil {
		return out, fmt.Errorf("rehearsal sources: %w", e)
	}
	settings, enabled, e := ResolveRoute(d.Input, map[string]any{"kind": "LOCAL_DRAFT", "object_id": string(d.Input.RootObjectID)}, "REHEARSAL", resolved, policy)
	if e != nil {
		return out, e
	}
	if !enabled {
		return out, fmt.Errorf("rehearsal route unavailable: %w", ErrBlocked)
	}
	all, e := objects(d.Input)
	if e != nil {
		return out, e
	}
	root := all[string(d.Input.RootObjectID)]
	slots := map[string]bool{}
	for _, v := range root["initial_actor_setups"].([]any) {
		slot := v.(map[string]any)["slot_id"].(string)
		slots[slot] = true
		out.ActorSlots = append(out.ActorSlots, slot)
	}
	if len(slots) == 0 || len(slots) > 32 {
		return out, ErrBlocked
	}
	for _, input := range request.Inputs {
		if !slots[string(input.ActorSlot)] {
			return out, ErrBlocked
		}
	}
	sort.Strings(out.ActorSlots)
	out.SourceHash, e = Hash(sources)
	if e != nil {
		return out, e
	}
	out.RouteHash, e = Hash(struct {
		Owner, Module string
		Settings      generated.GenerationSettings
	}{string(d.Input.RootObjectID), "REHEARSAL", settings})
	if e != nil {
		return out, e
	}
	out.Draft = d
	out.Resolved = resolved
	out.Sources = sources
	out.Settings = settings
	out.RoutePolicy = policy
	return out, nil
}

func (s *Service) BuildRehearsalTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, draft string, request generated.RehearsalRequest) (out domain.RehearsalBinding, err error) {
	m, e := s.BuildRehearsalDefinitionTx(ctx, tx, scope, draft, request)
	if e != nil {
		return out, e
	}
	revision, e := strconv.ParseUint(string(m.Draft.DraftRevision), 10, 64)
	if e != nil {
		return out, e
	}
	out = domain.RehearsalBinding{RoutePolicy: m.RoutePolicy, DraftID: draft, ProjectID: string(*m.Draft.ProjectID), Revision: revision, DraftHash: m.Draft.ContentHash, SourceHash: m.SourceHash, RouteHash: m.RouteHash, Sources: m.Sources, Settings: m.Settings, Assertions: append([]string{}, request.Assertions...)}
	for _, input := range request.Inputs {
		h, e := Hash(input)
		if e != nil {
			return domain.RehearsalBinding{}, e
		}
		out.Inputs = append(out.Inputs, domain.RehearsalInputBinding{ActorSlot: string(input.ActorSlot), Hash: h})
	}
	return out, nil
}

// Traverse only references used by this isolated program initialization or its
// source provenance. Display/media/theme references are not model input.
func rehearsalEdges(m map[string]any) []map[string]any {
	out := []map[string]any{}
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["kind"] == "LOCAL_DRAFT" || x["kind"] == "PINNED_DEFINITION" {
				out = append(out, x)
				return
			}
			keys := []string{}
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				switch k {
				case "asset_refs", "background_asset_ref", "appearance_asset_refs", "theme_ref":
					continue
				}
				walk(x[k])
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		}
	}
	walk(m)
	return out
}
