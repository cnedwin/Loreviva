package content

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"reflect"
	"time"
)

// Only a current source qualification failure retires derived bytes. SQL,
// planner and infrastructure failures remain retryable errors, never deletion.
func (s *Service) RehearsalSourcesLiveTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, b domain.RehearsalBinding) (bool, error) {
	e := s.InspectRehearsalSourcesTx(ctx, tx, scope, b)
	if e == nil {
		return true, nil
	}
	if errors.Is(e, ErrBlocked) || errors.Is(e, ErrNotFound) || errors.Is(e, sql.ErrNoRows) || errors.Is(e, store.ErrDenied) {
		return false, nil
	}
	return false, e
}

func (s *Service) PlanRehearsalHistoryLocks(ctx context.Context, scope domain.ProductScope, b domain.RehearsalBinding) ([]store.LockKey, error) {
	var raw []byte
	if e := s.Store.DB.QueryRowContext(ctx, "SELECT document FROM content_revisions WHERE tenant_id=? AND product_id=? AND draft_id=? AND revision=?", scope.TenantID, scope.ProductID, b.DraftID, b.Revision).Scan(&raw); e != nil {
		return nil, e
	}
	var old generated.ContentDraft
	if e := generated.Decode("ContentDraft", raw, &old); e != nil {
		return nil, e
	}
	keys, e := s.candidateSourceLocks(ctx, scope, old.Input)
	if e != nil {
		return nil, e
	}
	return append(keys, Key(scope, "content_draft", b.DraftID)), nil
}
func (s *Service) InspectRehearsalSourcesTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, b domain.RehearsalBinding) error {
	_, e := s.RehearsalHistoryDefinitionTx(ctx, tx, scope, b)
	return e
}

// S57 reconstructs only the actual immutable authoring baseline. Latest content
// and future routes are not substituted; current original source rights remain.
func (s *Service) RehearsalHistoryDefinitionTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, b domain.RehearsalBinding) (out RehearsalDefinition, err error) {
	if len(b.Sources) == 0 || len(b.Sources) > 10000 || len(b.SourceHash) != 64 {
		return out, ErrBlocked
	}
	if e := tx.RequireLock(Key(scope, "content_draft", b.DraftID)); e != nil {
		return out, e
	}
	var raw, currentRaw, storedHash []byte
	e := tx.QueryRowContext(ctx, "SELECT old.document,old.content_hash,cur.document FROM content_drafts d JOIN content_revisions old ON old.tenant_id=d.tenant_id AND old.product_id=d.product_id AND old.draft_id=d.draft_id AND old.revision=? JOIN content_revisions cur ON cur.tenant_id=d.tenant_id AND cur.product_id=d.product_id AND cur.draft_id=d.draft_id AND cur.revision=d.current_revision WHERE d.tenant_id=? AND d.product_id=? AND d.draft_id=? AND d.owner_subject_id=? AND d.deleted=0", b.Revision, scope.TenantID, scope.ProductID, b.DraftID, scope.SubjectID).Scan(&raw, &storedHash, &currentRaw)
	if e != nil {
		return out, e
	}
	var old, current generated.ContentDraft
	if generated.Decode("ContentDraft", raw, &old) != nil || generated.Decode("ContentDraft", currentRaw, &current) != nil || old.Scope != Scope(scope) || current.Scope != Scope(scope) || old.DraftID != generated.Identity(b.DraftID) || old.ContentHash != b.DraftHash || hex.EncodeToString(storedHash) != b.DraftHash {
		return out, ErrBlocked
	}
	h, e := Hash(old.Input)
	if e != nil || h != b.DraftHash {
		return out, ErrBlocked
	}
	sources, e := sourceGraphTx(ctx, tx, scope, old.Input, string(old.Input.RootObjectID), true)
	if e != nil {
		return out, e
	}
	h, e = Hash(sources)
	if e != nil || h != b.SourceHash || !reflect.DeepEqual(sources, b.Sources) {
		return out, ErrBlocked
	}
	objects, e := objects(current.Input)
	if e != nil {
		return out, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return out, e
	}
	for _, src := range sources {
		if src.ReleaseID != "" {
			continue
		}
		o := objects[src.ObjectID]
		if o == nil || o["kind"] != src.Kind {
			return out, ErrBlocked
		}
		if e = candidateRights(o, now); e != nil {
			return out, e
		}
		if src.AssetID != "" && (o["asset_id"] != src.AssetID || o["asset_sha256"] != src.AssetHash) {
			return out, ErrBlocked
		}
	}
	resolved, e := ResolvePinned(ctx, tx, scope, old.Input)
	if e != nil {
		return out, e
	}
	// Frozen route is verified against the frozen definition, never the latest.
	settings, on, e := ResolveRoute(old.Input, map[string]any{"kind": "LOCAL_DRAFT", "object_id": string(old.Input.RootObjectID)}, "REHEARSAL", resolved, b.RoutePolicy)
	if e != nil || !on || !reflect.DeepEqual(settings, b.Settings) {
		return out, ErrBlocked
	}
	route, e := Hash(struct {
		Owner, Module string
		Settings      generated.GenerationSettings
	}{string(old.Input.RootObjectID), "REHEARSAL", settings})
	if e != nil || route != b.RouteHash {
		return out, ErrBlocked
	}
	out = RehearsalDefinition{Draft: old, Resolved: resolved, Settings: settings, Sources: sources, SourceHash: b.SourceHash, RouteHash: b.RouteHash}
	return out, nil
}
