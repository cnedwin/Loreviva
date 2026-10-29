package content

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strings"
	"time"
)

func (s *Service) GetRelease(ctx context.Context, scope domain.ProductScope, id string) (generated.Release, error) {
	var r generated.Release
	if _, e := Gate(ctx, s.Store, scope); e != nil {
		return r, ErrNotFound
	}
	read, e := s.Store.BeginOperatorRead(ctx, scope, "content.read")
	if e != nil {
		return r, e
	}
	defer read.Close()
	var b []byte
	e = read.QueryRowContext(ctx, "SELECT c.document FROM content_releases c JOIN releases r ON r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.release_id=c.release_id WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=? AND r.available=1", scope.TenantID, scope.ProductID, id).Scan(&b)
	if e != nil {
		return r, ErrNotFound
	}
	e = generated.Decode("Release", b, &r)
	if e == nil {
		e = read.Finish(ctx)
	}
	if e != nil {
		return generated.Release{}, e
	}
	return r, e
}
func (s *Service) ListReleases(ctx context.Context, scope domain.ProductScope, project, draft string, options ...PageOptions) (generated.ReleasePage, error) {
	out := generated.ReleasePage{Items: []generated.Release{}}
	query := "SELECT c.release_id,CAST(c.draft_revision AS CHAR),c.content_hash FROM content_releases c JOIN content_drafts d ON d.tenant_id=c.tenant_id AND d.product_id=c.product_id AND d.draft_id=c.draft_id JOIN releases r ON r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.release_id=c.release_id WHERE c.tenant_id=? AND c.product_id=? AND d.owner_subject_id=? AND r.available=1 AND (?='' OR c.draft_id=?) AND (?='' OR d.project_id=?) ORDER BY c.release_id LIMIT 10001"
	b, next, e := s.page(ctx, scope, "releases", []string{project, draft}, firstOptions(options), query, []any{scope.TenantID, scope.ProductID, scope.SubjectID, draft, draft, project, project}, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", func(v pageEntry) []any { return []any{scope.TenantID, scope.ProductID, v.ID} })
	if e != nil {
		return out, e
	}
	out.NextCursor = next
	for _, raw := range b {
		var r generated.Release
		if e := generated.Decode("Release", raw, &r); e != nil {
			return out, e
		}
		out.Items = append(out.Items, r)
	}
	return out, nil
}
func dependencyLocks(scope domain.ProductScope, in generated.DraftInput) []store.LockKey {
	locks := []store.LockKey{}
	for _, dep := range in.DependencyLocks {
		locks = append(locks, Key(scope, "content_release", string(dep.ReleaseID)))
	}
	for _, raw := range in.Objects {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["kind"] == "ASSET" {
			if id, ok := m["asset_id"].(string); ok {
				locks = append(locks, Key(scope, "asset", id))
			}
		}
	}
	return locks
}
func validateDependencies(ctx context.Context, tx store.Tx, scope domain.ProductScope, in generated.DraftInput) ([]generated.FieldError, ResolvedDefinitions, error) {
	resolved, findings, e := resolvePinned(ctx, tx, scope, in)
	if e != nil {
		return nil, nil, e
	}
	add := func(p, c, m string) {
		findings = append(findings, generated.FieldError{Pointer: p, Code: generated.Identity(c), Message: m})
	}
	for _, raw := range in.Objects {
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		if m["kind"] != "ASSET" {
			continue
		}
		id := m["asset_id"].(string)
		if e := tx.RequireLock(Key(scope, "asset", id)); e != nil {
			return nil, nil, e
		}
		var b []byte
		var state string
		e := tx.QueryRowContext(ctx, "SELECT state,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, id).Scan(&state, &b)
		if errors.Is(e, sql.ErrNoRows) {
			add("/objects/"+id, "ASSET_NOT_READY", "Referenced private file is unavailable.")
			continue
		}
		if e != nil {
			return nil, nil, e
		}
		var record generated.FileRecord
		if e = generated.Decode("FileRecord", b, &record); e != nil {
			return nil, nil, e
		}
		if state != "READY" || record.Sha256 != m["asset_sha256"] || string(record.SizeBytes) != m["size_bytes"] {
			add("/objects/"+id, "ASSET_NOT_READY", "Referenced private file hash, size or readiness differs.")
		}
		if record.AudiencePolicy == "PRODUCT_EDITORS" {
			add("/objects/"+id, "ASSET_AUDIENCE", "Private editor file cannot be silently published to product readers.")
		}
		uses := map[string]bool{}
		for _, use := range record.Rights.AllowedUses {
			uses[use] = true
		}
		if !uses["PUBLISH"] || strings.TrimSpace(record.Rights.License) == "" {
			add("/objects/"+id+"/rights", "ASSET_SOURCE_RIGHTS", "Persisted upload must permit publication and retain a source license.")
		}
		if record.Rights.ExpiresAt != nil {
			add("/objects/"+id+"/rights", "ASSET_EXPIRING_SOURCE", "R0 fixed release requires nonexpiring upload source rights.")
		}
		for _, use := range m["rights"].(map[string]any)["allowed_uses"].([]any) {
			if !uses[use.(string)] {
				add("/objects/"+id+"/rights", "ASSET_SOURCE_RIGHTS", "Authored asset cannot enlarge persisted upload allowed uses.")
				break
			}
		}
	}
	return findings, resolved, nil
}
func support(labels []string) ([]generated.SupportLabel, error) {
	out := []generated.SupportLabel{}
	for _, s := range labels {
		if s != "MOCK_DEMO" {
			return nil, ErrBlocked
		}
		out = append(out, generated.SupportLabel(s))
	}
	if len(out) == 0 {
		out = append(out, "MOCK_DEMO")
	}
	return out, nil
}
func (s *Service) CheckPublish(ctx context.Context, scope domain.ProductScope, id string, expected uint64, labels []string, key string) (generated.PublishCheckReport, error) {
	var out generated.PublishCheckReport
	sl, e := support(labels)
	if e != nil {
		return out, e
	}
	snapshot, e := s.snapshotDraft(ctx, scope, id)
	if e != nil {
		return out, e
	}
	reportID := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "check", key)
	locks := append(dependencyLocks(scope, snapshot.Input), Key(scope, "content_draft", id), Key(scope, "content_check", reportID))
	locks = append(locks, store.RoutePolicyLocks(scope)...)
	raw, e := Command(ctx, s.Store, scope, "checkDraftForPublish", key, id, "PublishCheckReport", struct {
		Revision uint64
		Labels   []string
	}{expected, labels}, locks, func(tx store.Tx) (any, error) {
		d, e := current(ctx, tx, scope, id, expected)
		if e != nil {
			return nil, e
		}
		deps, resolved, e := validateDependencies(ctx, tx, scope, d.Input)
		policy, policyError := store.ReadRoutePolicyTx(ctx, tx, scope)
		if policyError != nil {
			return nil, policyError
		}
		f, duties := definitionFindingsPolicy(d.Input, string(d.RootKind), true, resolved, policy)
		if e != nil {
			return nil, e
		}
		f = append(f, deps...)
		_, routeHash, routeError := freezeRoutesWithPolicy(d.Input, resolved, policy)
		if routeError != nil {
			f = append(f, generated.FieldError{Pointer: "/generation", Code: "ROUTE_SNAPSHOT", Message: "Effective Mock route cannot be resolved."})
			routeHash, _ = Hash(d.Input)
		}
		r := generated.PublishCheckReport{RouteSnapshotHash: routeHash, ReportID: generated.Identity(reportID), Scope: Scope(scope), DraftID: d.DraftID, DraftRevision: d.DraftRevision, ContentHash: d.ContentHash, Passed: len(f) == 0, Findings: f, MockOnly: true, ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), SupportLabels: sl, RegisteredCapabilityIDS: []generated.Identity{"TEXT", "SIGN_CUSTODY"}, CoveredDutyIDS: []generated.Duty{}}
		for _, d := range duties {
			r.CoveredDutyIDS = append(r.CoveredDutyIDS, generated.Duty(d))
		}
		b, _ := json.Marshal(r)
		if e = tx.RequireLock(Key(scope, "content_check", reportID)); e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO content_checks(tenant_id,product_id,report_id,draft_id,draft_revision,report_kind,document) VALUES(?,?,?,?,?,'PUBLISH',?)", scope.TenantID, scope.ProductID, reportID, id, expected, b)
		return r, e
	})
	if e == nil {
		e = generated.Decode("PublishCheckReport", raw, &out)
	}
	return out, e
}

// A passed report is not sufficient to publish: the new transaction rechecks the draft, dependency rights and route snapshot before inserting the immutable release.
func (s *Service) Publish(ctx context.Context, scope domain.ProductScope, id string, expected uint64, reportID string, labels []string, key string) (generated.Release, error) {
	var out generated.Release
	sl, e := support(labels)
	if e != nil {
		return out, e
	}
	snapshot, e := s.snapshotDraft(ctx, scope, id)
	if e != nil {
		return out, e
	}
	releaseID := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "publish", key)
	locks := append(dependencyLocks(scope, snapshot.Input), Key(scope, "content_draft", id), Key(scope, "content_release", releaseID), Key(scope, "content_check", reportID))
	locks = append(locks, store.RoutePolicyLocks(scope)...)
	locks = append(locks, importLineageLocks(scope, id, snapshot.Input)...)
	raw, e := Command(ctx, s.Store, scope, "publishContentDraft", key, id, "Release", struct {
		Revision uint64
		Report   string
		Labels   []string
	}{expected, reportID, labels}, locks, func(tx store.Tx) (any, error) {
		d, e := current(ctx, tx, scope, id, expected)
		if e != nil {
			return nil, e
		}
		origins, e := importOrigins(ctx, tx, scope, "DRAFT", id)
		if e != nil {
			return nil, e
		}
		if len(origins) > 0 {
			return nil, ErrBlocked
		}
		for _, raw := range d.Input.Objects {
			var object struct {
				ID string `json:"object_id"`
			}
			json.Unmarshal(raw, &object)
			found, e := importOrigins(ctx, tx, scope, "OBJECT", object.ID)
			if e != nil {
				return nil, e
			}
			if len(found) > 0 {
				return nil, ErrBlocked
			}
		}
		if d.State != "DRAFT" {
			return nil, ErrBlocked
		}
		if e = tx.RequireLock(Key(scope, "content_check", reportID)); e != nil {
			return nil, e
		}
		var b []byte
		if e = tx.QueryRowContext(ctx, "SELECT document FROM content_checks WHERE tenant_id=? AND product_id=? AND report_id=? AND draft_id=? AND draft_revision=? AND report_kind='PUBLISH'", scope.TenantID, scope.ProductID, reportID, id, expected).Scan(&b); e != nil {
			return nil, ErrBlocked
		}
		var check generated.PublishCheckReport
		if e = generated.Decode("PublishCheckReport", b, &check); e != nil {
			return nil, e
		}
		expiry, e := time.Parse(time.RFC3339Nano, check.ExpiresAt)
		if e != nil || !check.Passed || check.ContentHash != d.ContentHash || !expiry.After(time.Now()) {
			return nil, ErrBlocked
		}
		deps, resolved, e := validateDependencies(ctx, tx, scope, d.Input)
		policy, policyError := store.ReadRoutePolicyTx(ctx, tx, scope)
		if policyError != nil {
			return nil, policyError
		}
		f, _ := definitionFindingsPolicy(d.Input, string(d.RootKind), true, resolved, policy)
		if e != nil {
			return nil, e
		}
		if len(f)+len(deps) > 0 {
			return nil, ErrBlocked
		}
		mappings, routeHash, e := freezeRoutesWithPolicy(d.Input, resolved, policy)
		if e != nil || routeHash != check.RouteSnapshotHash {
			return nil, ErrBlocked
		}
		r := generated.Release{Scope: Scope(scope), ReleaseID: generated.Identity(releaseID), DraftID: d.DraftID, ProjectID: d.ProjectID, DraftRevision: d.DraftRevision, Input: d.Input, DependencyLocks: d.Input.DependencyLocks, PublishedBySubjectID: generated.Identity(scope.SubjectID), PublishedAt: now(), CheckReportID: generated.Identity(reportID), SupportLabels: sl, MockOnly: true}
		r.RouteMappings = mappings
		r.ContentHash, e = Hash(struct {
			Input        generated.DraftInput
			Dependencies []generated.DependencyLock
			Mappings     any
		}{r.Input, r.DependencyLocks, r.RouteMappings})
		if e != nil {
			return nil, e
		}
		b, e = json.Marshal(r)
		if e != nil {
			return nil, e
		}
		hash, _ := hex.DecodeString(r.ContentHash)
		if e = tx.RequireLock(Key(scope, "content_release", releaseID)); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO releases(tenant_id,product_id,release_id,available) VALUES(?,?,?,1)", scope.TenantID, scope.ProductID, releaseID); e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO content_releases(tenant_id,product_id,release_id,draft_id,draft_revision,content_hash,document) VALUES(?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, releaseID, id, expected, hash, b)
		return r, e
	})
	if e == nil {
		e = generated.Decode("Release", raw, &out)
	}
	return out, e
}
