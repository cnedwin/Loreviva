package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"time"
)

// The provision can only be minted from the existing committed terminal owner.
// Its fields are private; HTTP input or a caller-supplied report cannot mint it.
type PackageApprovalProvision struct {
	store       *Store
	scope       domain.ProductScope
	history     *rehearsalHistoryPlan
	draft, hash string
	revision    uint64
	files       PackageApprovalFileProof
	assertions  map[string]string
}

type PackageApprovalFileProof interface {
	CheckPackageApprovalFiles(context.Context, domain.ProductScope, string, CommittedRehearsalReport) error
	Close()
}
type PackageApprovalFilesOwner interface {
	HoldPackageApprovalFiles(context.Context, domain.ProductScope, string) (PackageApprovalFileProof, []byte, error)
}
type packageApprovalCheck struct {
	draft, report, input, route string
	revision                    uint64
	raw                         []byte
}

func (s *Store) ArmPackageApprovalCheck(ctx context.Context, tx Tx, scope domain.ProductScope, draft, report string, revision uint64, input, route string) error {
	u, ok := tx.(*unit)
	if !ok || u.operatorStore != s || u.scope != scope || !u.commandOnly || u.operation != "approvePackageDraft" || u.packageApprovalCheck != nil {
		return ErrDenied
	}
	p := &packageApprovalCheck{draft: draft, report: report, revision: revision, input: input, route: route}
	if e := s.inspectPackageApprovalCheck(ctx, u, p); e != nil {
		return e
	}
	u.packageApprovalCheck = p
	return nil
}
func (s *Store) inspectPackageApprovalCheck(ctx context.Context, u *unit, p *packageApprovalCheck) error {
	if u.operatorStore != s || u.operation != "approvePackageDraft" {
		return ErrDenied
	}
	if e := u.RequireLock(LockKey{Rank: 90, Kind: "content_check", IDs: []string{u.scope.TenantID, u.scope.ProductID, p.report}}); e != nil {
		return e
	}
	var raw []byte
	var clock time.Time
	if e := u.QueryRowContext(ctx, "SELECT document FROM content_checks WHERE tenant_id=? AND product_id=? AND draft_id=? AND draft_revision=? AND report_id=? AND report_kind='PUBLISH'", u.scope.TenantID, u.scope.ProductID, p.draft, p.revision, p.report).Scan(&raw); e != nil {
		return e
	}
	if p.raw != nil && !bytes.Equal(raw, p.raw) {
		return ErrDenied
	}
	var report generated.PublishCheckReport
	if generated.Decode("PublishCheckReport", raw, &report) != nil || !report.Passed || report.Scope != (generated.ProductScope{TenantID: u.scope.TenantID, ProductID: u.scope.ProductID}) || string(report.DraftID) != p.draft || string(report.DraftRevision) != fmt.Sprint(p.revision) || string(report.ReportID) != p.report || report.ContentHash != p.input || report.RouteSnapshotHash != p.route {
		return ErrDenied
	}
	expires, e := time.Parse(time.RFC3339Nano, report.ExpiresAt)
	if e != nil {
		return ErrDenied
	}
	if e = u.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&clock); e != nil {
		return e
	}
	if !expires.After(clock) {
		return ErrDenied
	}
	if p.raw == nil {
		p.raw = append([]byte(nil), raw...)
	}
	return nil
}
func (p *PackageApprovalProvision) Close() {
	if p != nil && p.files != nil {
		p.files.Close()
	}
}

func (p *PackageApprovalProvision) Locks() []LockKey {
	if p == nil || p.history == nil {
		return nil
	}
	keys := append([]LockKey{}, p.history.lockList...)
	keys = append(keys, LockKey{Rank: 40, IDs: []string{p.scope.TenantID, p.scope.ProductID, p.history.init.world}}, harnessTaskKey(p.scope, p.history.init.root))
	for _, child := range p.history.children {
		keys = append(keys, harnessTaskKey(p.scope, child.TaskID))
	}
	return keys
}
func (p *PackageApprovalProvision) CommandBinding() CommandBarrierRequest {
	return CommandBarrierRequest{packageApproval: p}
}

func (s *Store) PreparePackageApproval(ctx context.Context, scope domain.ProductScope, task, draft, hash string, revision uint64) (*PackageApprovalProvision, error) {
	if s.Guard == nil || s.DraftRehearsalOwner == nil || s.PackageApprovalFiles == nil {
		return nil, ErrDenied
	}
	files, body, e := s.PackageApprovalFiles.HoldPackageApprovalFiles(ctx, scope, task)
	if e != nil {
		return nil, e
	}
	kept := false
	defer func() {
		if !kept {
			files.Close()
		}
	}()
	if generated.Validate("RehearsalReportArtifact", body) != nil {
		return nil, ErrDenied
	}
	original, e := s.InspectCommittedRehearsalReport(ctx, scope, task)
	if e != nil {
		return nil, e
	}
	p, e := s.prepareRehearsalHistoryPlan(ctx, scope, task)
	if e != nil {
		return nil, e
	}
	sum := sha256.Sum256(body)
	if p.init.binding.DraftID != draft || p.init.binding.DraftHash != hash || p.init.binding.Revision != revision || hex.EncodeToString(sum[:]) != p.hash || original.Storage != p.storage {
		return nil, ErrDenied
	}
	var report struct {
		Inputs []struct {
			Index int    `json:"index"`
			State string `json:"state"`
		} `json:"inputs"`
		Assertions []domain.RehearsalAssertion `json:"assertions"`
	}
	if json.Unmarshal(body, &report) != nil || len(report.Inputs) == 0 || len(report.Inputs) != len(p.init.binding.Inputs) {
		return nil, ErrDenied
	}
	for i, input := range report.Inputs {
		if input.Index != i || input.State != "COMMITTED" {
			return nil, ErrDenied
		}
	}
	needed := map[string]bool{"ISOLATED": true, "RIGHTS": true, "DUTIES": true}
	assertions := map[string]string{}
	seen := map[string]bool{}
	for _, a := range report.Assertions {
		if seen[a.Kind] {
			return nil, ErrDenied
		}
		seen[a.Kind] = true
		assertions[a.Kind] = a.State
		if needed[a.Kind] && a.State != "PASS" {
			return nil, ErrDenied
		}
		if a.State == "FAIL" {
			return nil, ErrDenied
		}
	}
	for kind := range needed {
		if !seen[kind] {
			return nil, ErrDenied
		}
	}
	kept = true
	return &PackageApprovalProvision{s, scope, p, draft, hash, revision, files, assertions}, nil
}

// Only lock acquisition uses the historical read view. The actual approval
// unit keeps its own write operation and never receives a reusable history flag.
func packageHistoryLockView(r BarrierRequest) BarrierRequest {
	if p := r.packageApproval; p != nil && p.history != nil {
		r.rehearsalHistory = p.history
		r.TaskID = p.history.init.root
	}
	return r
}
func (s *Store) inspectPackageApprovalFinish(ctx context.Context, u *unit, p *PackageApprovalProvision) error {
	if p == nil || p.store != s || p.scope != u.scope || u.operatorStore != s || u.operation != "approvePackageDraft" || !u.commandOnly || u.rehearsalHistory != nil {
		return ErrDenied
	}
	original, e := s.inspectRehearsalHistoryPlanTx(ctx, u, p.history)
	if e != nil {
		return e
	}
	if original.Storage != p.history.storage {
		return ErrDenied
	}
	if p.files == nil {
		return ErrDenied
	}
	if e = p.files.CheckPackageApprovalFiles(ctx, p.scope, p.history.init.root, original); e != nil {
		return e
	}
	// Applicability comes from the actual frozen runtime rule program, rather
	// than guessing from the presence of a prop or treating NOT_RUN as PASS.
	if len(p.history.children) == 0 {
		return ErrDenied
	}
	last := p.history.children[len(p.history.children)-1]
	view := *u
	view.taskID = last.TaskID
	view.world = p.history.init.world
	view.rehearsalHistory = p.history
	view.rehearsalReportRead = true
	view.rehearsalExecution = last.rehearsalExecution
	required, e := s.RehearsalInitializer.PackageRehearsalRequiresSignTx(ctx, &view, domain.Scope{ProductScope: p.scope, WorldInstanceID: p.history.init.world})
	if e != nil {
		return e
	}
	if required && p.assertions["SIGN_CUSTODY"] != "PASS" {
		return ErrDenied
	}
	var currentBinding, currentInput, hash, evidence []byte
	var state string
	var deleted bool
	e = u.QueryRowContext(ctx, "SELECT a.binding,i.document,r.body_hash,r.evidence_hash,r.state,r.delete_intent FROM harness_admin_rehearsals a JOIN harness_rehearsal_input_sources i USING(tenant_id,product_id,task_id,draft_id) JOIN harness_rehearsal_reports r USING(tenant_id,product_id,task_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? AND a.owner_subject_id=?", p.scope.TenantID, p.scope.ProductID, p.history.init.root, p.scope.SubjectID).Scan(&currentBinding, &currentInput, &hash, &evidence, &state, &deleted)
	if e != nil {
		return e
	}
	if state != "COMMITTED" || deleted || !bytes.Equal(currentBinding, p.history.bindingRaw) || !bytes.Equal(currentInput, p.history.inputRaw) || string(hash) != p.history.hash || !bytes.Equal(evidence, p.history.evidenceHash) {
		return ErrDenied
	}
	policy, e := ReadRoutePolicyTx(ctx, u, u.scope)
	if e != nil {
		return e
	}
	a, _ := json.Marshal(policy)
	b, _ := json.Marshal(p.history.init.binding.RoutePolicy)
	if !bytes.Equal(a, b) {
		return ErrDenied
	}
	return u.err
}

type PackageExportOwner interface {
	PlanPackageExportLocks(context.Context, domain.ProductScope, string) ([]LockKey, error)
	InspectPackageExportTx(context.Context, Tx, domain.ProductScope, string) (string, error)
	MaterializePackageExportTx(context.Context, Tx, domain.ProductScope, string) (PackageExportMaterial, error)
}
type PackageExportMaterial struct {
	Root    generated.Release
	Catalog map[string]generated.Release
	Assets  map[string]generated.FileRecord
	Basis   string
}

func (s *Store) RequirePackageExportAssetRead(ctx context.Context, tx Tx, scope domain.ProductScope) error {
	u, ok := tx.(*unit)
	if !ok || u.operatorStore != s || u.scope != scope || (u.operation != "exportContentPackage" && u.operation != "PACKAGE_EXPORT_READ") {
		return ErrDenied
	}
	if e := CheckOperatorTx(ctx, tx, scope, "asset.read"); e != nil {
		return e
	}
	for _, a := range u.memoryHTTPActions {
		if a == "asset.read" {
			return nil
		}
	}
	u.memoryHTTPActions = append(u.memoryHTTPActions, "asset.read")
	return nil
}
func (s *Store) InspectPackageExportSource(ctx context.Context, scope domain.ProductScope, release string) (PackageExportMaterial, error) {
	var out PackageExportMaterial
	if s.PackageExportOwner == nil {
		return out, ErrDenied
	}
	keys, e := s.PackageExportOwner.PlanPackageExportLocks(ctx, scope, release)
	if e != nil {
		return out, e
	}
	var gates GateVersions
	var enabled bool
	e = s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,t.enabled AND p.enabled AND g.enabled FROM subject_governance g JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled)
	if e != nil || !enabled {
		return out, ErrDenied
	}
	key := turnIdentity(release, "package-export-inspect")
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "PACKAGE_EXPORT_READ", CommandKey: key, CommitKey: key, GateVersions: gates, PayloadHash: sha256.Sum256([]byte(release)), Locks: keys, commandOnly: true, packageTransferRead: true}
	done := errors.New("export source inspected without mutation")
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		var e error
		out, e = s.PackageExportOwner.MaterializePackageExportTx(ctx, tx, scope, release)
		if e != nil {
			return e
		}
		u := tx.(*unit)
		if e = s.checkFinish(ctx, u.tx, r, u); e != nil {
			return e
		}
		basis, e := s.PackageExportOwner.InspectPackageExportTx(ctx, tx, scope, release)
		if e != nil {
			return e
		}
		if basis != out.Basis {
			return ErrConflict
		}
		return done
	})
	if !errors.Is(e, done) {
		return PackageExportMaterial{}, e
	}
	return out, nil
}

type packageExportInspection struct{ release, basis string }

func (s *Store) ArmPackageExportInspection(ctx context.Context, tx Tx, scope domain.ProductScope, release, basis string) error {
	u, ok := tx.(*unit)
	if !ok || u.operatorStore != s || u.scope != scope || !u.commandOnly || u.operation != "exportContentPackage" || u.packageExportInspection != nil || s.PackageExportOwner == nil {
		return ErrDenied
	}
	frozen := &packageExportInspection{release, basis}
	if e := s.inspectPackageExportFinish(ctx, u, frozen); e != nil {
		return e
	}
	u.packageExportInspection = frozen
	return nil
}
func (s *Store) inspectPackageExportFinish(ctx context.Context, u *unit, p *packageExportInspection) error {
	if u.operatorStore != s || s.PackageExportOwner == nil {
		return ErrDenied
	}
	basis, e := s.PackageExportOwner.InspectPackageExportTx(ctx, u, u.scope, p.release)
	if e != nil {
		return e
	}
	if basis != p.basis {
		return ErrConflict
	}
	return u.err
}

// ReadExport owns an entirely rolled-back, write-disabled transaction. It
// reuses the same final source inspection as production export commits.
func (s *Store) ReadPackageExport(ctx context.Context, scope domain.ProductScope, id string) (generated.PackageExportReceipt, []byte, error) {
	var out generated.PackageExportReceipt
	var body []byte
	read, e := s.BeginOperatorRead(ctx, scope, "content.read", "content.copy")
	if e != nil {
		return out, nil, e
	}
	var raw []byte
	e = read.QueryRowContext(ctx, "SELECT document FROM content_package_exports WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND export_id=?", scope.TenantID, scope.ProductID, scope.SubjectID, id).Scan(&raw)
	if e == nil {
		e = generated.Decode("PackageExportReceipt", raw, &out)
	}
	if e == nil {
		e = read.Finish(ctx)
	}
	read.Close()
	if e != nil {
		return out, nil, e
	}
	if s.PackageExportOwner == nil {
		return out, nil, ErrDenied
	}
	keys, e := s.PackageExportOwner.PlanPackageExportLocks(ctx, scope, string(out.ReleaseID))
	if e != nil {
		return out, nil, e
	}
	keys = append(keys, LockKey{Rank: 90, Kind: "content_check", IDs: []string{scope.TenantID, scope.ProductID, id}})
	var gates GateVersions
	var enabled bool
	e = s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,t.enabled AND p.enabled AND g.enabled FROM subject_governance g JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled)
	if e != nil || !enabled {
		return out, nil, ErrDenied
	}
	key := turnIdentity(id, "package-export-read")
	digest := sha256.Sum256([]byte(id))
	r := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: "PACKAGE_EXPORT_READ", CommandKey: key, CommitKey: key, GateVersions: gates, PayloadHash: digest, Locks: keys, commandOnly: true, packageTransferRead: true}
	done := errors.New("export inspected without a write")
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		var stored, sourceHash, archiveHash []byte
		if e := u.RequireLock(LockKey{Rank: 90, Kind: "content_check", IDs: []string{scope.TenantID, scope.ProductID, id}}); e != nil {
			return e
		}
		if e := tx.QueryRowContext(ctx, "SELECT document,source_hash,archive_hash,archive FROM content_package_exports WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND export_id=?", scope.TenantID, scope.ProductID, scope.SubjectID, id).Scan(&stored, &sourceHash, &archiveHash, &body); e != nil {
			return e
		}
		if !bytes.Equal(stored, raw) {
			return ErrDenied
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != out.ArchiveSha256 || !bytes.Equal(sum[:], archiveHash) {
			return ErrDenied
		}
		p := &packageExportInspection{string(out.ReleaseID), hex.EncodeToString(sourceHash)}
		if e := s.inspectPackageExportFinish(ctx, u, p); e != nil {
			return e
		}
		if e := s.checkFinish(ctx, u.tx, r, u); e != nil {
			return e
		}
		if e := s.inspectPackageExportFinish(ctx, u, p); e != nil {
			return e
		}
		return done
	})
	if !errors.Is(e, done) {
		return generated.PackageExportReceipt{}, nil, e
	}
	return out, body, nil
}
