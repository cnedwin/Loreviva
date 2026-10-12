package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// DraftGeneration is an internal owning projection, not a public wire schema
// or a reusable permission token. Consumers must recheck inside their own UoW.
type DraftGeneration = domain.DraftGeneration

func (s *Service) InspectDraftGeneration(ctx context.Context, scope domain.ProductScope, draft string, revision uint64, object, module string) (DraftGeneration, error) {
	return s.Store.InspectDraftGeneration(ctx, scope, domain.DraftCandidateSelection{DraftID: draft, Revision: revision, ObjectID: object, ModuleID: module})
}

// Only lock identities escape this fixed preplanner; current read/use is proved in the UoW.
func (s *Service) PlanDraftGenerationLocks(ctx context.Context, scope domain.ProductScope, selection domain.DraftCandidateSelection) ([]store.LockKey, error) {
	var raw []byte
	e := s.Store.DB.QueryRowContext(ctx, "SELECT r.document FROM content_drafts d JOIN content_revisions r ON r.tenant_id=d.tenant_id AND r.product_id=d.product_id AND r.draft_id=d.draft_id AND r.revision=d.current_revision WHERE d.tenant_id=? AND d.product_id=? AND d.draft_id=? AND d.owner_subject_id=? AND d.deleted=0", scope.TenantID, scope.ProductID, selection.DraftID, scope.SubjectID).Scan(&raw)
	if e != nil {
		return nil, e
	}
	var d generated.ContentDraft
	if e = generated.Decode("ContentDraft", raw, &d); e != nil {
		return nil, e
	}
	keys, e := s.candidateSourceLocks(ctx, scope, d.Input)
	if e != nil {
		return nil, e
	}
	return append(keys, Key(scope, "content_draft", selection.DraftID)), nil
}
func (s *Service) InspectDraftGenerationTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, selection domain.DraftCandidateSelection) (DraftGeneration, error) {
	out, e := draftGenerationCoreTx(ctx, tx, scope, selection.DraftID, selection.Revision, selection.ObjectID, selection.ModuleID)
	if e != nil {
		return out, e
	}
	if _, e = candidateSourcesTx(ctx, tx, scope, out.Draft.Input, selection.ObjectID); e != nil {
		return DraftGeneration{}, e
	}
	return out, nil
}

// Only the installed Store program path supplies bytes from its owning Files
// evidence. Validation never applies candidate fields to the real draft.
func (s *Service) ValidateCandidateTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, selection domain.DraftCandidateSelection, body []byte) (string, error) {
	resolved, e := CandidateGenerationTx(ctx, tx, scope, selection.DraftID, selection.Revision, selection.ObjectID, selection.ModuleID)
	if e != nil {
		return "", e
	}
	v, e := wire.Parse(body)
	if e != nil {
		return "STRUCTURE_INVALID", nil
	}
	object, ok := v.(map[string]any)
	if !ok {
		return "STRUCTURE_INVALID", nil
	}
	if refusal, ok := object["refusal"].(string); ok && refusal != "" {
		return "REFUSAL", nil
	}
	fields, ok := object["fields"].(map[string]any)
	if !ok || len(object) != 1 || len(fields) != len(selection.Fields) {
		return "STRUCTURE_INVALID", nil
	}
	all, e := objects(resolved.Draft.Input)
	if e != nil {
		return "", e
	}
	target := all[selection.ObjectID]
	if target == nil {
		return "", ErrNotFound
	}
	allowed := map[string]bool{}
	for _, f := range resolved.Fields {
		allowed[f] = true
	}
	seen := map[string]bool{}
	for _, f := range selection.Fields {
		value, present := fields[f]
		if !allowed[f] || seen[f] || !present {
			return "STRUCTURE_INVALID", nil
		}
		seen[f] = true
		target[f] = value
	}
	raw, e := json.Marshal(target)
	if e != nil {
		return "", e
	}
	if generated.Validate("ContentObject", raw) != nil {
		return "STRUCTURE_INVALID", nil
	}
	return "STRUCTURE_VALID", nil
}

func (s *Service) PlanCandidateLocks(ctx context.Context, scope domain.ProductScope, selection domain.DraftCandidateSelection) ([]store.LockKey, error) {
	d, e := s.snapshotDraft(ctx, scope, selection.DraftID)
	if e != nil {
		return nil, e
	}
	keys, e := s.candidateSourceLocks(ctx, scope, d.Input)
	if e != nil {
		return nil, e
	}
	return append(keys, Key(scope, "content_draft", selection.DraftID)), nil
}
func (s *Service) BuildCandidateTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, selection domain.DraftCandidateSelection) (domain.DraftCandidateMaterial, error) {
	var out domain.DraftCandidateMaterial
	resolved, e := CandidateGenerationTx(ctx, tx, scope, selection.DraftID, selection.Revision, selection.ObjectID, selection.ModuleID)
	if e != nil {
		return out, e
	}
	if resolved.State != "RESOLVED" || len(selection.Fields) == 0 || len(selection.Fields) > 30 {
		return out, ErrBlocked
	}
	supported := map[string]bool{}
	for _, f := range resolved.Fields {
		supported[f] = true
	}
	selected := map[string]bool{}
	for _, f := range selection.Fields {
		if !supported[f] || selected[f] {
			return out, ErrBlocked
		}
		selected[f] = true
	}
	all, e := objects(resolved.Draft.Input)
	if e != nil {
		return out, e
	}
	values := map[string]json.RawMessage{}
	for f := range selected {
		raw, e := json.Marshal(all[selection.ObjectID][f])
		if e != nil {
			return out, e
		}
		values[f] = raw
	}
	selection.Fields = append([]string{}, selection.Fields...)
	sort.Strings(selection.Fields)
	settings, e := json.Marshal(resolved.Settings)
	if e != nil {
		return out, e
	}
	outputBound := uint64(store.MockOutputLimit)
	if resolved.Settings.Parameters != nil && resolved.Settings.Parameters.MaxOutputTokens != nil {
		limit := *resolved.Settings.Parameters.MaxOutputTokens
		if limit <= 0 {
			return out, ErrBlocked
		}
		if uint64(limit) < outputBound {
			outputBound = uint64(limit)
		}
	}
	sources, e := candidateSourcesTx(ctx, tx, scope, resolved.Draft.Input, selection.ObjectID)
	if e != nil {
		return out, e
	}
	sourceHash, e := Hash(sources)
	if e != nil {
		return out, e
	}
	prompt := domain.DraftCandidatePrompt{Selection: selection, DraftHash: resolved.Draft.ContentHash, RouteHash: resolved.RouteHash, SourceHash: sourceHash, Values: values, Settings: settings}
	raw, e := json.Marshal(prompt)
	if e != nil {
		return out, e
	}
	canonical, e := wire.Canonical(raw)
	if e != nil {
		return out, e
	}
	if !utf8.Valid(canonical) || utf8.RuneCount(canonical) > store.MockInputLimit {
		return out, ErrBlocked
	}
	project := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "R0_AUTHORING_PROJECT")
	if resolved.Draft.ProjectID != nil {
		project = string(*resolved.Draft.ProjectID)
	}
	hash := sha256.Sum256(canonical)
	return domain.DraftCandidateMaterial{Selection: selection, DraftHash: resolved.Draft.ContentHash, RouteHash: resolved.RouteHash, RequestHash: hex.EncodeToString(hash[:]), ProjectID: project, Connection: MockConnection, Model: MockModel, Canonical: canonical, InputBound: store.MockInputLimit, OutputBound: outputBound, SourceHash: sourceHash, Sources: sources}, nil
}

// CandidateGenerationTx is the fixed owner of draft generation qualification.
// It reads only authoring definitions. It never authorizes WORLD memory use.
func CandidateGenerationTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, draft string, revision uint64, object, module string) (DraftGeneration, error) {
	var out DraftGeneration
	if scope.TenantID != "demo-local" || scope.ProductID != "coffee" || scope.SubjectID != "creator" {
		return out, store.ErrDenied
	}
	var allowed bool
	e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM operator_credentials c JOIN operator_actions a ON a.credential_id=c.credential_id WHERE c.credential_id='owner' AND c.tenant_id=? AND c.product_id=? AND c.subject_id=? AND c.enabled=1 AND a.action='content.assist')", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&allowed)
	if e != nil {
		return out, e
	}
	if !allowed {
		return out, store.ErrDenied
	}
	return draftGenerationCoreTx(ctx, tx, scope, draft, revision, object, module)
}

func draftGenerationCoreTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, draft string, revision uint64, object, module string) (DraftGeneration, error) {
	var out DraftGeneration
	d, e := current(ctx, tx, scope, draft, revision)
	if e != nil {
		return out, e
	}
	if d.State != "DRAFT" && d.State != "ISOLATED_IMPORT" {
		return out, ErrBlocked
	}
	computed, e := Hash(d.Input)
	if e != nil || computed != d.ContentHash {
		return out, ErrBlocked
	}
	resolved, e := ResolvePinned(ctx, tx, scope, d.Input)
	if e != nil {
		return out, e
	}
	all, e := objects(d.Input)
	if e != nil {
		return out, e
	}
	owner := all[object]
	if owner == nil {
		return out, ErrNotFound
	}
	var rights generated.SourceRights
	raw, _ := json.Marshal(owner["rights"])
	if json.Unmarshal(raw, &rights) != nil {
		return out, ErrBlocked
	}
	edit := false
	for _, use := range rights.AllowedUses {
		edit = edit || use == "EDIT"
	}
	if !edit || strings.TrimSpace(rights.License) == "" {
		return out, ErrBlocked
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return out, e
	}
	if rights.ExpiresAt != nil {
		expiry, e := time.Parse(time.RFC3339Nano, *rights.ExpiresAt)
		if e != nil || !now.Before(expiry) {
			return out, ErrBlocked
		}
	}
	kind, _ := owner["kind"].(string)
	out = DraftGeneration{Draft: d, ObjectID: object, ModuleID: module, OwnerKind: kind, State: "BLOCKED", Fields: []string{}}
	// Fixed program owners are never editing-model handlers. NONE is their
	// actual configuration, not a route fallback or procurement authorization.
	if (kind == "ROLE" || kind == "SKILL") && owner["implementation_kind"] == "PROGRAM" {
		raw, err := json.Marshal(owner["generation"])
		if err != nil || generated.Decode("GenerationSettings", raw, &out.Settings) != nil || out.Settings.Mode != "NONE" {
			return out, nil
		}
		out.State = "NONE"
		return out, nil
	}
	switch module {
	case "AUTHORING":
		out.Fields = []string{"description"}
		if kind == "WORLD" {
			out.Fields = append(out.Fields, "background")
		}
		if kind == "CHARACTER" {
			out.Fields = append(out.Fields, "biography", "expression_style")
		}
		if kind == "RULE" {
			out.Fields = append(out.Fields, "reason")
		}
	case "INTERACTION":
		if kind == "CHARACTER" {
			out.Fields = []string{"expression_style", "example_lines"}
		}
	case "PROACTIVE", "REHEARSAL", "EVALUATION":
		if kind == "ROLE" && owner["implementation_kind"] == "AGENT" {
			out.Fields = []string{"instruction_template"}
		}
	case "KNOWLEDGE_EXTRACTION":
		if kind == "KNOWLEDGE" {
			out.Fields = []string{"text"}
		}
	}
	if len(out.Fields) == 0 {
		return out, nil
	}
	settings, enabled, e := effectiveDraftCandidateRoute(d.Input, object, module, resolved)
	out.Settings = settings
	if e != nil {
		return out, nil
	}
	if !enabled {
		out.State = "NONE"
		return out, nil
	}
	out.RouteHash, e = Hash(struct {
		Owner, Module            string
		Settings                 generated.GenerationSettings
		Connection, Model, Price string
	}{object, module, settings, MockConnection, MockModel, "0"})
	if e != nil {
		return DraftGeneration{}, e
	}
	out.State = "RESOLVED"
	return out, nil
}
