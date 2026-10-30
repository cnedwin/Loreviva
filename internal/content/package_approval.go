package content

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"math"
	"reflect"
	"sort"
	"strconv"
	"time"
)

type PackageFileReader interface {
	ReadPackageExportAsset(context.Context, domain.ProductScope, string) ([]byte, generated.FileRecord, error)
	ReadCommittedRehearsalReport(context.Context, domain.ProductScope, string) ([]byte, error)
}
type packageAdoption struct {
	DraftID, Owner, Revision, InputHash, SourceHash, RouteHash, Check, Rehearsal, Note string
	Origins                                                                            []string
}

func sortedOrigins(origins map[string]bool) []string {
	keys := []string{}
	for k := range origins {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func adoptionKey(scope domain.ProductScope, id string) store.LockKey {
	return Key(scope, "content_check", Identity("package-adoption", id))
}

func (s *Service) hasTransferResult(ctx context.Context, scope domain.ProductScope, operation, key string) (bool, error) {
	if domain.ValidateIdempotencyKey(key) != nil {
		return false, ErrBlocked
	}
	read, e := s.Store.BeginOperatorRead(ctx, scope, "content.read")
	if e != nil {
		return false, e
	}
	defer read.Close()
	var n int
	e = read.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_results WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND result_id=?", scope.TenantID, scope.ProductID, scope.SubjectID, Identity(scope.TenantID, scope.ProductID, scope.SubjectID, operation, key)).Scan(&n)
	if e != nil {
		return false, e
	}
	if e = read.Finish(ctx); e != nil {
		return false, e
	}
	return n == 1, nil
}
func (s *Service) ApprovePackageDraft(ctx context.Context, scope domain.ProductScope, id string, request generated.PackageApprovalRequest, key string) (generated.ContentDraft, error) {
	var out generated.ContentDraft
	raw, _ := json.Marshal(request)
	if generated.Validate("PackageApprovalRequest", raw) != nil || string(request.ProductID) != scope.ProductID {
		return out, ErrBlocked
	}
	if found, e := s.hasTransferResult(ctx, scope, "approvePackageDraft", key); e != nil {
		return out, e
	} else if found {
		saved, e := BoundCommand(ctx, s.Store, scope, "approvePackageDraft", key, id, "ContentDraft", request, nil, store.CommandBarrierRequest{}, func(store.Tx) (any, error) { return nil, ErrBlocked })
		if e == nil {
			e = generated.Decode("ContentDraft", saved, &out)
		}
		return out, e
	}
	revision, e := Revision(string(request.ExpectedDraftRevision))
	if e != nil || revision == math.MaxUint64 {
		return out, ErrBlocked
	}
	snapshot, e := s.snapshotDraft(ctx, scope, id)
	if e != nil {
		return out, e
	}
	locks, e := s.PlanPackagePreviewLocks(ctx, scope, snapshot.Input, string(snapshot.RootKind))
	if e != nil {
		return out, e
	}
	locks = append(locks, importLineageLocks(scope, id, snapshot.Input)...)
	locks = append(locks, Key(scope, "content_draft", id), adoptionKey(scope, id), Key(scope, "content_check", string(request.CheckReportID)))
	var provision *store.PackageApprovalProvision
	if snapshot.RootKind == "WORLD" {
		if request.RehearsalTaskID == nil || s.PackageFiles == nil {
			return out, ErrBlocked
		}
		provision, e = s.Store.PreparePackageApproval(ctx, scope, string(*request.RehearsalTaskID), id, snapshot.ContentHash, revision)
		if e != nil {
			return out, e
		}
		defer provision.Close()
		locks = append(locks, provision.Locks()...)
	} else if request.RehearsalTaskID != nil {
		return out, ErrBlocked
	}
	binding := store.CommandBarrierRequest{}
	if provision != nil {
		binding = provision.CommandBinding()
	}
	saved, e := BoundCommand(ctx, s.Store, scope, "approvePackageDraft", key, id, "ContentDraft", request, locks, binding, func(tx store.Tx) (any, error) {
		draft, e := current(ctx, tx, scope, id, revision)
		if e != nil {
			return nil, e
		}
		if draft.State != "ISOLATED_IMPORT" {
			return nil, ErrBlocked
		}
		origins, e := importOrigins(ctx, tx, scope, "DRAFT", id)
		if e != nil || len(origins) == 0 {
			return nil, ErrBlocked
		}
		if e = tx.RequireLock(Key(scope, "content_check", string(request.CheckReportID))); e != nil {
			return nil, e
		}
		var raw []byte
		e = tx.QueryRowContext(ctx, "SELECT document FROM content_checks WHERE tenant_id=? AND product_id=? AND draft_id=? AND draft_revision=? AND report_id=? AND report_kind='PUBLISH'", scope.TenantID, scope.ProductID, id, revision, request.CheckReportID).Scan(&raw)
		if e != nil {
			return nil, ErrBlocked
		}
		var report generated.PublishCheckReport
		if generated.Decode("PublishCheckReport", raw, &report) != nil || !report.Passed || report.Scope != Scope(scope) || report.ContentHash != draft.ContentHash {
			return nil, ErrBlocked
		}
		expires, e := time.Parse(time.RFC3339Nano, report.ExpiresAt)
		if e != nil || !expires.After(time.Now()) {
			return nil, ErrBlocked
		}
		inspection, e := s.InspectPackagePreviewTx(ctx, tx, scope, draft.Input, string(draft.RootKind), nil, draft.RootKind == "WORLD")
		if e != nil || len(inspection.Findings) > 0 || inspection.RouteHash != report.RouteSnapshotHash {
			return nil, ErrBlocked
		}
		if e = s.Store.ArmPackageApprovalCheck(ctx, tx, scope, id, string(request.CheckReportID), revision, draft.ContentHash, inspection.RouteHash); e != nil {
			return nil, e
		}
		if e = s.Store.ArmPackageImportInspection(ctx, tx, scope, draft.Input, string(draft.RootKind), nil, draft.RootKind == "WORLD"); e != nil {
			return nil, e
		}
		draft.State = "DRAFT"
		draft.DraftRevision = generated.ExactUnsigned(strconv.FormatUint(revision+1, 10))
		// This digest identifies the checked static source projection, not a grant.
		// The owning transaction independently rechecks current authority at finish.
		sourceHash, err := Hash(struct {
			Input generated.DraftInput
			Route string
		}{inspection.Input, inspection.RouteHash})
		if err != nil {
			return nil, err
		}
		proof := packageAdoption{DraftID: id, Owner: scope.SubjectID, Revision: string(draft.DraftRevision), InputHash: draft.ContentHash, SourceHash: sourceHash, RouteHash: inspection.RouteHash, Check: string(request.CheckReportID), Note: request.ApprovalNote, Origins: sortedOrigins(origins)}
		if request.RehearsalTaskID != nil {
			proof.Rehearsal = string(*request.RehearsalTaskID)
		}
		document, _ := json.Marshal(proof)
		hash, _ := hex.DecodeString(draft.ContentHash)
		if e = tx.RequireLock(adoptionKey(scope, id)); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO content_package_adoptions(tenant_id,product_id,draft_id,revision,owner_subject_id,content_hash,document) VALUES(?,?,?,?,?,?,?)", scope.TenantID, scope.ProductID, id, revision+1, scope.SubjectID, hash, document); e != nil {
			return nil, e
		}
		if e = writeRevision(ctx, tx, scope, draft); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE content_drafts SET current_revision=?,state='DRAFT' WHERE tenant_id=? AND product_id=? AND draft_id=?", revision+1, scope.TenantID, scope.ProductID, id); e != nil {
			return nil, e
		}
		return draft, nil
	})
	if e == nil {
		e = generated.Decode("ContentDraft", saved, &out)
	}
	return out, e
}

// Publication consumes an exact revision approval, never a mutable source_ref
// or a document state. Protected origins are kept throughout the approval.
func (s *Service) requirePackageAdoption(ctx context.Context, tx store.Tx, scope domain.ProductScope, draft generated.ContentDraft) error {
	origins, e := importOrigins(ctx, tx, scope, "DRAFT", string(draft.DraftID))
	if e != nil {
		return e
	}
	for _, raw := range draft.Input.Objects {
		var object struct {
			ID string `json:"object_id"`
		}
		json.Unmarshal(raw, &object)
		found, e := importOrigins(ctx, tx, scope, "OBJECT", object.ID)
		if e != nil {
			return e
		}
		for id := range found {
			origins[id] = true
		}
	}
	if len(origins) == 0 {
		return nil
	}
	if e = tx.RequireLock(adoptionKey(scope, string(draft.DraftID))); e != nil {
		return e
	}
	var raw, hash []byte
	e = tx.QueryRowContext(ctx, "SELECT document,content_hash FROM content_package_adoptions WHERE tenant_id=? AND product_id=? AND draft_id=? AND revision=? AND owner_subject_id=?", scope.TenantID, scope.ProductID, draft.DraftID, draft.DraftRevision, scope.SubjectID).Scan(&raw, &hash)
	if e != nil {
		return ErrBlocked
	}
	var proof packageAdoption
	if json.Unmarshal(raw, &proof) != nil || proof.DraftID != string(draft.DraftID) || proof.Owner != scope.SubjectID || proof.Revision != string(draft.DraftRevision) || proof.InputHash != draft.ContentHash || hex.EncodeToString(hash) != draft.ContentHash || !reflect.DeepEqual(proof.Origins, sortedOrigins(origins)) {
		return ErrBlocked
	}
	inspection, e := s.InspectPackagePreviewTx(ctx, tx, scope, draft.Input, string(draft.RootKind), nil, draft.RootKind == "WORLD")
	if e != nil || len(inspection.Findings) > 0 {
		return ErrBlocked
	}
	sourceHash, e := Hash(struct {
		Input generated.DraftInput
		Route string
	}{inspection.Input, inspection.RouteHash})
	if e != nil || proof.SourceHash != sourceHash || proof.RouteHash != inspection.RouteHash || (draft.RootKind == "WORLD" && proof.Rehearsal == "") {
		return ErrBlocked
	}
	if e = s.Store.ArmPackageImportInspection(ctx, tx, scope, draft.Input, string(draft.RootKind), nil, draft.RootKind == "WORLD"); e != nil {
		return e
	}
	return nil
}
