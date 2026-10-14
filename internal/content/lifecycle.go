package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
	"time"
)

func (s *Service) Impact(ctx context.Context, scope domain.ProductScope, id string, expected uint64, action, key string, selected ...string) (generated.ImpactReport, error) {
	var out generated.ImpactReport
	reportID := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "impact", key)
	snapshot, e := s.snapshotDraft(ctx, scope, id)
	if e != nil {
		return out, e
	}
	locks := []store.LockKey{Key(scope, "content_draft", id), Key(scope, "content_check", reportID)}
	locks = append(locks, dependencyLocks(scope, snapshot.Input)...)
	raw, e := Command(ctx, s.Store, scope, "checkDraftImpact", key, id, "ImpactReport", struct {
		Revision uint64
		Action   string
		Selected []string
	}{expected, action, selected}, locks, func(tx store.Tx) (any, error) {
		d, e := current(ctx, tx, scope, id, expected)
		if e != nil {
			return nil, e
		}
		r := generated.ImpactReport{ReportID: generated.Identity(reportID), Scope: Scope(scope), DraftID: d.DraftID, DraftRevision: d.DraftRevision, ContentHash: d.ContentHash, Action: generated.DraftAction(action), AffectedReleaseIDS: []generated.Identity{}, BlockingFindings: []generated.FieldError{}, ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)}
		resolved, dependencyFindings, e := resolvePinned(ctx, tx, scope, d.Input)
		if e != nil {
			return nil, e
		}
		r.SelectedTargets, r.IncomingRefs, r.DutyCoverage, r.BlockingFindings, e = staticImpact(d, action, selected, resolved)
		r.BlockingFindings = append(r.BlockingFindings, dependencyFindings...)
		if e != nil {
			return nil, e
		}
		var refs []byte
		e = tx.QueryRowContext(ctx, "SELECT COALESCE(JSON_ARRAYAGG(CAST(release_id AS CHAR)),JSON_ARRAY()) FROM content_releases WHERE tenant_id=? AND product_id=? AND draft_id=?", scope.TenantID, scope.ProductID, id).Scan(&refs)
		if e != nil {
			return nil, e
		}
		if e = json.Unmarshal(refs, &r.AffectedReleaseIDS); e != nil {
			return nil, e
		}
		if action == "DELETE" && len(r.AffectedReleaseIDS) > 0 && len(targetSet(r.SelectedTargets, "OBJECT")) == len(d.Input.Objects) {
			r.BlockingFindings = append(r.BlockingFindings, generated.FieldError{Pointer: "/action", Code: "REFERENCED_RELEASE", Message: "Retained immutable releases reference this draft. Archive it instead."})
		}
		b, _ := json.Marshal(r)
		if e = tx.RequireLock(Key(scope, "content_check", reportID)); e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO content_checks(tenant_id,product_id,report_id,draft_id,draft_revision,report_kind,document) VALUES(?,?,?,?,?,'IMPACT',?)", scope.TenantID, scope.ProductID, reportID, id, expected, b)
		return r, e
	})
	if e == nil {
		e = generated.Decode("ImpactReport", raw, &out)
	}
	return out, e
}
func (s *Service) Command(ctx context.Context, scope domain.ProductScope, id string, expected uint64, action, reportID, restoreRevision, copyName, key string) (json.RawMessage, error) {
	// Precollect only safe owner-scoped metadata, including a retained tombstone.
	// A successful DELETE/COPY replay must reach its original receipt before the
	// non-replay body checks current visibility or revision.
	if _, e := Gate(ctx, s.Store, scope); e != nil {
		return nil, e
	}
	var project sql.NullString
	if action == "COPY" {
		if e := s.Store.DB.QueryRowContext(ctx, "SELECT project_id FROM content_drafts WHERE tenant_id=? AND product_id=? AND draft_id=? AND owner_subject_id=?", scope.TenantID, scope.ProductID, id, scope.SubjectID).Scan(&project); e != nil {
			return nil, ErrNotFound
		}
	}
	newID := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "copy", key)
	newProject := Identity(newID, "project")
	locks := []store.LockKey{Key(scope, "content_draft", id), Key(scope, "content_check", reportID)}
	if action == "REMOVE_BINDING" {
		snapshot, e := s.snapshotDraft(ctx, scope, id)
		if e != nil {
			return nil, e
		}
		locks = append(locks, dependencyLocks(scope, snapshot.Input)...)
	}
	if action == "COPY" {
		locks = append(locks, Key(scope, "content_draft", newID))
		if project.Valid {
			locks = append(locks, Key(scope, "content_project", newProject))
		}
	}
	return Command(ctx, s.Store, scope, "commandContentDraft", key, id, "DraftCommandResult", struct {
		Revision                      uint64
		Action, Report, Restore, Name string
	}{expected, action, reportID, restoreRevision, copyName}, locks, func(tx store.Tx) (any, error) {
		// A new lifecycle effect must match the stored impact report's revision, hash, action and expiry. Binding removal recomputes its selected targets, and whole-draft deletion still refuses retained releases.
		d, e := current(ctx, tx, scope, id, expected)
		if e != nil {
			return nil, e
		}
		if e = tx.RequireLock(Key(scope, "content_check", reportID)); e != nil {
			return nil, e
		}
		var b []byte
		if e = tx.QueryRowContext(ctx, "SELECT document FROM content_checks WHERE tenant_id=? AND product_id=? AND report_id=? AND draft_id=? AND draft_revision=? AND report_kind='IMPACT'", scope.TenantID, scope.ProductID, reportID, id, expected).Scan(&b); e != nil {
			return nil, ErrBlocked
		}
		var report generated.ImpactReport
		if e = generated.Decode("ImpactReport", b, &report); e != nil {
			return nil, e
		}
		expiry, e := time.Parse(time.RFC3339Nano, report.ExpiresAt)
		if e != nil || !expiry.After(time.Now()) || report.ContentHash != d.ContentHash || string(report.Action) != action || len(report.BlockingFindings) > 0 {
			return nil, ErrBlocked
		}
		if action == "COPY" {
			in, kind, e := copyInput(d, report.SelectedTargets)
			if e != nil {
				return nil, e
			}
			if copyName != "" {
				in.Name = copyName
			}
			in, e = Prepare(in)
			if e != nil {
				return nil, e
			}
			project := ""
			if d.ProjectID != nil && kind == "WORLD" {
				project = newProject
			}
			copy := makeDraft(scope, newID, project, kind, in)
			if e = createRows(ctx, tx, scope, copy, project != ""); e != nil {
				return nil, e
			}
			return generated.CopyReceipt{SourceDraftID: d.DraftID, Draft: copy, ExcludedCategories: []string{"RUNTIME_IDENTITY", "PLAYER_STATE", "PRIVATE_MEMORY", "COSTS", "CREDENTIALS"}}, nil
		}
		if expected == ^uint64(0) {
			return nil, ErrBlocked
		}
		switch action {
		case "REMOVE_BINDING":
			selected := make([]string, 0, len(report.SelectedTargets))
			for _, target := range report.SelectedTargets {
				if target.Kind != "MODULE_BINDING" && target.Kind != "SKILL_BINDING" {
					return nil, ErrBlocked
				}
				selected = append(selected, string(target.TargetID))
			}
			resolved, dependencyFindings, e := resolvePinned(ctx, tx, scope, d.Input)
			if e != nil {
				return nil, e
			}
			targets, _, _, findings, e := staticImpact(d, action, selected, resolved)
			if e != nil || len(findings) > 0 || len(dependencyFindings) > 0 {
				return nil, ErrBlocked
			}
			actual, _ := json.Marshal(targets)
			planned, _ := json.Marshal(report.SelectedTargets)
			if string(actual) != string(planned) {
				return nil, ErrBlocked
			}
			d.Input, e = Prepare(selectedInput(d.Input, targets, false))
			if e != nil {
				return nil, e
			}
		case "ARCHIVE":
			d.State = "ARCHIVED"
		case "RESTORE":
			if restoreRevision != "" && restoreRevision != "0" {
				if _, e = Revision(restoreRevision); e != nil {
					return nil, ErrBlocked
				}
				if e = tx.QueryRowContext(ctx, "SELECT document FROM content_revisions WHERE tenant_id=? AND product_id=? AND draft_id=? AND revision=?", scope.TenantID, scope.ProductID, id, restoreRevision).Scan(&b); e != nil {
					return nil, ErrBlocked
				}
				var historical generated.ContentDraft
				if e = generated.Decode("ContentDraft", b, &historical); e != nil {
					return nil, e
				}
				d.Input, e = Prepare(historical.Input)
				if e != nil {
					return nil, e
				}
			}
			d.State = "DRAFT"
		case "DELETE":
			if len(targetSet(report.SelectedTargets, "OBJECT")) < len(d.Input.Objects) {
				d.Input = selectedInput(d.Input, report.SelectedTargets, false)
				if _, e = Prepare(d.Input); e != nil {
					return nil, e
				}
				break
			}
			var n int
			if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM content_releases WHERE tenant_id=? AND product_id=? AND draft_id=?", scope.TenantID, scope.ProductID, id).Scan(&n); e != nil {
				return nil, e
			}
			if n > 0 {
				return nil, ErrBlocked
			}
			if e = tx.RequireLock(Key(scope, "content_draft", id)); e != nil {
				return nil, e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE content_drafts SET deleted=1 WHERE tenant_id=? AND product_id=? AND draft_id=?", scope.TenantID, scope.ProductID, id); e != nil {
				return nil, e
			}
			if e = tx.PurgeRehearsalInputs(ctx, scope, id); e != nil {
				return nil, e
			}
			return generated.DraftCommandReceipt{DraftID: d.DraftID, Action: generated.DraftAction(action), DraftRevision: d.DraftRevision, State: "DELETED"}, nil
		default:
			return nil, ErrBlocked
		}
		d.DraftRevision = generated.ExactUnsigned(strconv.FormatUint(expected+1, 10))
		d.ModifiedAt = now()
		d.ContentHash, e = Hash(d.Input)
		if e != nil {
			return nil, e
		}
		f, _ := Findings(d.Input, string(d.RootKind))
		d.ValidationSummary = generated.ValidationSummary{BlockingCount: generated.ExactUnsigned(strconv.Itoa(len(f))), Findings: f}
		if e = tx.RequireLock(Key(scope, "content_draft", id)); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE content_drafts SET current_revision=?,state=? WHERE tenant_id=? AND product_id=? AND draft_id=?", expected+1, d.State, scope.TenantID, scope.ProductID, id); e != nil {
			return nil, e
		}
		if e = writeRevision(ctx, tx, scope, d); e != nil {
			return nil, e
		}
		return generated.DraftCommandReceipt{DraftID: d.DraftID, Action: generated.DraftAction(action), DraftRevision: d.DraftRevision, State: d.State}, nil
	})
}
