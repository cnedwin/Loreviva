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
	"strconv"
	"time"
)

func now() string                       { return time.Now().UTC().Format(time.RFC3339Nano) }
func Revision(s string) (uint64, error) { return strconv.ParseUint(s, 10, 64) }
func (s *Service) GetDraft(ctx context.Context, scope domain.ProductScope, id, revision string) (generated.ContentDraft, error) {
	var d generated.ContentDraft
	if _, e := Gate(ctx, s.Store, scope); e != nil {
		return d, ErrNotFound
	}
	q := "SELECT r.document FROM content_drafts d JOIN content_revisions r ON r.tenant_id=d.tenant_id AND r.product_id=d.product_id AND r.draft_id=d.draft_id AND r.revision="
	if revision == "" {
		q += "d.current_revision"
	} else {
		if _, e := Revision(revision); e != nil {
			return d, ErrNotFound
		}
		q += "?"
	}
	q += " WHERE d.tenant_id=? AND d.product_id=? AND d.draft_id=? AND d.owner_subject_id=? AND d.deleted=0"
	args := []any{}
	if revision != "" {
		args = append(args, revision)
	}
	args = append(args, scope.TenantID, scope.ProductID, id, scope.SubjectID)
	var b []byte
	if e := s.Store.DB.QueryRowContext(ctx, q, args...).Scan(&b); e != nil {
		return d, ErrNotFound
	}
	if e := generated.Decode("ContentDraft", b, &d); e != nil {
		return d, e
	}
	return d, nil
}

// Safe immutable metadata precollection may include a tombstone; visibility,
// expected revision and business state are checked only after receipt replay.
func (s *Service) snapshotDraft(ctx context.Context, scope domain.ProductScope, id string) (generated.ContentDraft, error) {
	var d generated.ContentDraft
	if _, e := Gate(ctx, s.Store, scope); e != nil {
		return d, ErrNotFound
	}
	var b []byte
	e := s.Store.DB.QueryRowContext(ctx, "SELECT r.document FROM content_drafts d JOIN content_revisions r ON r.tenant_id=d.tenant_id AND r.product_id=d.product_id AND r.draft_id=d.draft_id AND r.revision=d.current_revision WHERE d.tenant_id=? AND d.product_id=? AND d.draft_id=? AND d.owner_subject_id=?", scope.TenantID, scope.ProductID, id, scope.SubjectID).Scan(&b)
	if e != nil {
		return d, ErrNotFound
	}
	e = generated.Decode("ContentDraft", b, &d)
	return d, e
}
func current(ctx context.Context, tx store.Tx, scope domain.ProductScope, id string, expected uint64) (generated.ContentDraft, error) {
	var d generated.ContentDraft
	var b []byte
	if e := tx.RequireLock(Key(scope, "content_draft", id)); e != nil {
		return d, e
	}
	e := tx.QueryRowContext(ctx, "SELECT r.document FROM content_drafts d JOIN content_revisions r ON r.tenant_id=d.tenant_id AND r.product_id=d.product_id AND r.draft_id=d.draft_id AND r.revision=d.current_revision WHERE d.tenant_id=? AND d.product_id=? AND d.draft_id=? AND d.owner_subject_id=? AND d.deleted=0", scope.TenantID, scope.ProductID, id, scope.SubjectID).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if e != nil {
		return d, e
	}
	if e = generated.Decode("ContentDraft", b, &d); e != nil {
		return d, e
	}
	if string(d.DraftRevision) != strconv.FormatUint(expected, 10) {
		return d, &Conflict{id, strconv.FormatUint(expected, 10), string(d.DraftRevision), d.ContentHash}
	}
	return d, nil
}
func writeRevision(ctx context.Context, tx store.Tx, scope domain.ProductScope, d generated.ContentDraft) error {
	if e := tx.RequireLock(Key(scope, "content_draft", string(d.DraftID))); e != nil {
		return e
	}
	b, e := json.Marshal(d)
	if e != nil {
		return e
	}
	if e = generated.Validate("ContentDraft", b); e != nil {
		return e
	}
	h, e := hex.DecodeString(d.ContentHash)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO content_revisions(tenant_id,product_id,draft_id,revision,content_hash,document) VALUES(?,?,?,?,?,?)", scope.TenantID, scope.ProductID, d.DraftID, d.DraftRevision, h, b)
	return e
}
func makeDraft(scope domain.ProductScope, id, project, kind string, in generated.DraftInput) generated.ContentDraft {
	hash, _ := Hash(in)
	findings, _ := Findings(in, kind)
	d := generated.ContentDraft{Scope: Scope(scope), DraftID: generated.Identity(id), DraftRevision: "1", RootKind: generated.RootContentKind(kind), State: "DRAFT", Input: in, ContentHash: hash, AuthorSubjectID: generated.Identity(scope.SubjectID), ModifiedBySubjectID: generated.Identity(scope.SubjectID), CreatedAt: now(), ModifiedAt: now(), ValidationSummary: generated.ValidationSummary{BlockingCount: generated.ExactUnsigned(strconv.Itoa(len(findings))), Findings: findings}}
	if project != "" {
		p := generated.Identity(project)
		d.ProjectID = &p
	}
	return d
}
func createRows(ctx context.Context, tx store.Tx, scope domain.ProductScope, d generated.ContentDraft, newProject bool) error {
	var p any
	if d.ProjectID != nil {
		p = *d.ProjectID
		if e := tx.RequireLock(Key(scope, "content_project", string(*d.ProjectID))); e != nil {
			return e
		}
		if newProject {
			if _, e := tx.ExecContext(ctx, "INSERT INTO world_projects(tenant_id,product_id,project_id,owner_subject_id,name) VALUES(?,?,?,?,?)", scope.TenantID, scope.ProductID, p, scope.SubjectID, d.Input.Name); e != nil {
				return e
			}
		} else {
			var owner string
			e := tx.QueryRowContext(ctx, "SELECT owner_subject_id FROM world_projects WHERE tenant_id=? AND product_id=? AND project_id=?", scope.TenantID, scope.ProductID, p).Scan(&owner)
			if e != nil || owner != scope.SubjectID {
				return ErrNotFound
			}
		}
	}
	if e := tx.RequireLock(Key(scope, "content_draft", string(d.DraftID))); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "INSERT INTO content_drafts(tenant_id,product_id,draft_id,project_id,owner_subject_id,root_kind,current_revision,state) VALUES(?,?,?,?,?,?,1,?)", scope.TenantID, scope.ProductID, d.DraftID, p, scope.SubjectID, d.RootKind, d.State); e != nil {
		return e
	}
	return writeRevision(ctx, tx, scope, d)
}
func (s *Service) Create(ctx context.Context, scope domain.ProductScope, kind, project string, in generated.DraftInput, key string) (generated.ContentDraft, error) {
	var out generated.ContentDraft
	in, e := Prepare(in)
	if e != nil {
		return out, e
	}
	id := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "create", key)
	newProject := kind == "WORLD" && project == ""
	if newProject {
		project = Identity(id, "project")
	}
	d := makeDraft(scope, id, project, kind, in)
	locks := []store.LockKey{Key(scope, "content_draft", id)}
	if project != "" {
		locks = append(locks, Key(scope, "content_project", project))
	}
	raw, e := Command(ctx, s.Store, scope, "createContentDraft", key, "", "ContentDraft", struct {
		Kind, Project string
		Input         generated.DraftInput
	}{kind, project, in}, locks, func(tx store.Tx) (any, error) {
		if e := createRows(ctx, tx, scope, d, newProject); e != nil {
			return nil, e
		}
		return d, nil
	})
	if e == nil {
		e = generated.Decode("ContentDraft", raw, &out)
	}
	return out, e
}
func (s *Service) SaveDraft(ctx context.Context, scope domain.ProductScope, id string, in generated.DraftInput, expected uint64, key string) (generated.ContentDraft, error) {
	var out generated.ContentDraft
	in, e := Prepare(in)
	if e != nil {
		return out, e
	}
	raw, e := Command(ctx, s.Store, scope, "saveContentDraft", key, id, "ContentDraft", struct {
		Revision uint64
		Input    generated.DraftInput
	}{expected, in}, []store.LockKey{Key(scope, "content_draft", id)}, func(tx store.Tx) (any, error) {
		d, e := current(ctx, tx, scope, id, expected)
		if e != nil {
			return nil, e
		}
		if d.State != "DRAFT" && d.State != "ISOLATED_IMPORT" {
			return nil, ErrBlocked
		}
		if expected == ^uint64(0) {
			return nil, ErrBlocked
		}
		d.Input = in
		d.ContentHash, e = Hash(in)
		if e != nil {
			return nil, e
		}
		d.DraftRevision = generated.ExactUnsigned(strconv.FormatUint(expected+1, 10))
		d.ModifiedAt = now()
		d.ModifiedBySubjectID = generated.Identity(scope.SubjectID)
		f, _ := Findings(in, string(d.RootKind))
		d.ValidationSummary = generated.ValidationSummary{BlockingCount: generated.ExactUnsigned(strconv.Itoa(len(f))), Findings: f}
		if e = tx.RequireLock(Key(scope, "content_draft", id)); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE content_drafts SET current_revision=? WHERE tenant_id=? AND product_id=? AND draft_id=?", expected+1, scope.TenantID, scope.ProductID, id); e != nil {
			return nil, e
		}
		if e = writeRevision(ctx, tx, scope, d); e != nil {
			return nil, e
		}
		return d, nil
	})
	if e == nil {
		e = generated.Decode("ContentDraft", raw, &out)
	}
	return out, e
}
func (s *Service) ListDrafts(ctx context.Context, scope domain.ProductScope, project, kind, state string, limit int, cursors ...string) (generated.DraftPage, error) {
	opt := PageOptions{Limit: limit}
	if len(cursors) > 0 {
		opt.Cursor = cursors[0]
	}
	query := "SELECT d.draft_id,CAST(d.current_revision AS CHAR),r.content_hash FROM content_drafts d JOIN content_revisions r ON r.tenant_id=d.tenant_id AND r.product_id=d.product_id AND r.draft_id=d.draft_id AND r.revision=d.current_revision WHERE d.tenant_id=? AND d.product_id=? AND d.owner_subject_id=? AND d.deleted=0 AND (?='' OR d.project_id=?) AND (?='' OR d.root_kind=?) AND (?='' OR d.state=?) ORDER BY d.draft_id LIMIT 10001"
	b, next, e := s.page(ctx, scope, "drafts", []string{project, kind, state}, opt, query, []any{scope.TenantID, scope.ProductID, scope.SubjectID, project, project, kind, kind, state, state}, "SELECT document FROM content_revisions WHERE tenant_id=? AND product_id=? AND draft_id=? AND revision=?", func(v pageEntry) []any { return []any{scope.TenantID, scope.ProductID, v.ID, v.Revision} })
	if e != nil {
		return generated.DraftPage{}, e
	}
	return decodeDraftPage(b, next)
}
func (s *Service) Revisions(ctx context.Context, scope domain.ProductScope, id string, options ...PageOptions) (generated.DraftRevisionPage, error) {
	out := generated.DraftRevisionPage{Items: []generated.DraftRevisionSummary{}}
	if _, e := s.GetDraft(ctx, scope, id, ""); e != nil {
		return out, e
	}
	b, next, e := s.page(ctx, scope, "revisions", []string{id}, firstOptions(options), "SELECT CAST(revision AS CHAR),CAST(revision AS CHAR),content_hash FROM content_revisions WHERE tenant_id=? AND product_id=? AND draft_id=? ORDER BY revision DESC LIMIT 10001", []any{scope.TenantID, scope.ProductID, id}, "SELECT document FROM content_revisions WHERE tenant_id=? AND product_id=? AND draft_id=? AND revision=?", func(v pageEntry) []any { return []any{scope.TenantID, scope.ProductID, id, v.Revision} })
	if e != nil {
		return out, e
	}
	out.NextCursor = next
	for _, raw := range b {
		var d generated.ContentDraft
		if e := generated.Decode("ContentDraft", raw, &d); e != nil {
			return out, e
		}
		out.Items = append(out.Items, generated.DraftRevisionSummary{DraftID: d.DraftID, DraftRevision: d.DraftRevision, ContentHash: d.ContentHash, ModifiedAt: d.ModifiedAt, ModifiedBySubjectID: d.ModifiedBySubjectID, State: d.State})
	}
	return out, nil
}
