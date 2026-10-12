package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"strconv"
	"time"
)

// Report text is a safe program projection, never a copy of input/dialogue or memory.
type rehearsalReportDocument struct {
	Program    string                   `json:"program"`
	Inputs     []rehearsalReportOutcome `json:"inputs"`
	Assertions []rehearsalAssertion     `json:"assertions"`
}
type rehearsalReportOutcome struct {
	Index    int    `json:"index"`
	ResultID string `json:"result_id"`
	State    string `json:"state"`
	Supplier string `json:"supplier_reconciliation_state"`
}
type rehearsalAssertion = domain.RehearsalAssertion

func reportOperation(op string) bool {
	return op == "REHEARSAL_REPORT_INSPECT" || op == "REHEARSAL_REPORT_STAGE" || op == "REHEARSAL_REPORT_COMMIT"
}

// This recognizes only this exact owning transaction's already applied typed
// report and joint receipt. It cannot mint a read/resume privilege from a row.
func checkRehearsalReportApplied(ctx context.Context, u *unit) error {
	p := u.rehearsalReport
	if p == nil || p.applied == nil || p.applied.tx != u.tx || p.applied.rehearsalReport != p {
		return ErrDenied
	}
	if e := checkRehearsalReportStageIdentity(ctx, u); e != nil {
		return e
	}
	key := turnIdentity(p.root.TaskID, "report-commit")
	var valid bool
	e := u.tx.QueryRowContext(ctx, "SELECT a.own_steps=2 AND a.fsm_state='COMMITTED' AND r.state='COMMITTED' AND r.storage_key=? AND r.body_hash=? AND r.asset_id=? AND r.delete_intent=0 AND c.operation='REHEARSAL_REPORT_COMMIT' AND c.command_key=? AND c.payload_hash=r.commit_hash AND c.phase='COMMIT' AND c.scope_kind='PRODUCT' AND c.world_instance_id IS NULL AND c.owner_subject_id=? AND z.state='READY' AND z.storage_key=r.storage_key FROM harness_rehearsal_reports r JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) JOIN commit_receipts c ON c.commit_key=r.receipt_key AND c.tenant_id=r.tenant_id AND c.product_id=r.product_id AND c.task_id=r.task_id JOIN assets z ON z.tenant_id=r.tenant_id AND z.product_id=r.product_id AND z.asset_id=r.asset_id WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND r.receipt_key=?", p.artifact.Storage, p.artifact.Hash, turnIdentity(p.root.TaskID, "report-asset"), key, p.root.SubjectID, p.root.TenantID, p.root.ProductID, p.root.TaskID, key).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	service := turnIdentity(p.root.TaskID, "service")
	if e := checkOriginalServiceMeteringTx(ctx, u, p.root.ProductScope, p.root.TaskID, service, key, reportCosts(p)); e != nil {
		return e
	}
	for _, h := range p.holds {
		state := reportHoldState(p, h)
		if state == "HELD" {
			continue
		}
		if e = u.tx.QueryRowContext(ctx, "SELECT task_id=? AND owner_subject_id=? AND kind=? AND scope_key=? AND account_id=? AND amount=0 AND status=? FROM postings WHERE tenant_id=? AND product_id=? AND record_id=?", h.task, p.root.SubjectID, h.Kind, h.ScopeKey, h.AccountID, state, p.root.TenantID, p.root.ProductID, turnIdentity(p.root.TaskID, "posting", h.ReservationID)).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrDenied
		}
	}
	return nil
}

func reportDocument(r BarrierRequest) ([]byte, error) {
	p := r.rehearsalReport
	if p == nil || len(p.expected) != len(r.rehearsalInitialization.request.Inputs) {
		return nil, ErrDenied
	}
	d := rehearsalReportDocument{Program: "R0_REHEARSAL_REPORT_V1", Inputs: []rehearsalReportOutcome{}, Assertions: []rehearsalAssertion{}}
	for i, c := range p.expected {
		final := c.Metadata.UsageKnown && c.Metadata.NoFurtherCost
		if c.OriginalDependency != nil {
			final = final && c.OriginalDependency.UsageKnown && c.OriginalDependency.NoFurtherCost
		}
		state := "PENDING_VERIFICATION"
		if final {
			state = "RECONCILED"
		}
		outcome := "COMMITTED"
		if c.Negative != nil {
			outcome = c.Negative.State
		}
		d.Inputs = append(d.Inputs, rehearsalReportOutcome{i, c.ResultID, outcome, state})
	}
	if len(p.assertions) != len(r.rehearsalInitialization.request.Assertions) {
		return nil, ErrDenied
	}
	for i, finding := range p.assertions {
		if finding.Kind != r.rehearsalInitialization.request.Assertions[i] {
			return nil, ErrDenied
		}
		d.Assertions = append(d.Assertions, finding)
	}
	return json.Marshal(d)
}

func (s *Store) BindRehearsalReportStage(ctx context.Context, evidence RehearsalReportEvidence, root domain.TaskLease, worker string) error {
	r, e := s.planRehearsalReport(ctx, evidence, root, worker)
	if e != nil {
		return e
	}
	ctx, e = s.RehearsalReportReader.RehearsalReportContext(evidence, root, worker)
	if e != nil {
		return e
	}
	a, e := s.RehearsalReportReader.RehearsalReportArtifact(evidence, root, worker)
	if e != nil {
		return e
	}
	if len(a.Storage) != 32 || len(a.Hash) != 64 {
		return ErrDenied
	}
	if _, e = hex.DecodeString(a.Storage); e != nil {
		return ErrDenied
	}
	r.Operation = "REHEARSAL_REPORT_STAGE"
	r.CommandKey = turnIdentity(root.TaskID, "report-stage")
	r.CommitKey = r.CommandKey
	r.Locks = append(r.Locks, LockKey{Rank: 90, Kind: "asset", IDs: []string{root.TenantID, root.ProductID, "staging." + a.Storage}})
	r.rehearsalReport.keys[string(lockBytes(r.Locks[len(r.Locks)-1]))] = true
	_, r.PayloadHash, e = harnessPayload(struct {
		Storage, Hash string
		Privacy, View uint64
	}{a.Storage, a.Hash, r.GateVersions.Privacy, r.GateVersions.View})
	if e != nil {
		return e
	}
	_, e = s.WithBarrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if e := s.inspectRehearsalReportTx(ctx, u, r); e != nil {
			return e
		}
		body, e := reportDocument(r)
		if e != nil {
			return e
		}
		hash := sha256.Sum256(body)
		if !bytes.Equal(body, a.Body) || hex.EncodeToString(hash[:]) != a.Hash {
			return ErrConflict
		}
		_, proof, e := harnessPayload(r.rehearsalReport.expected)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO file_staging(tenant_id,product_id,storage_key,owner_subject_id) VALUES(?,?,?,?)", root.TenantID, root.ProductID, a.Storage, root.SubjectID); e != nil {
			return e
		}
		_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_rehearsal_reports(tenant_id,product_id,task_id,storage_key,body_hash,evidence_hash,stage_receipt,subject_privacy,subject_view) VALUES(?,?,?,?,?,?,?,?,?)", root.TenantID, root.ProductID, root.TaskID, a.Storage, a.Hash, proof[:], r.CommitKey, r.GateVersions.Privacy, r.GateVersions.View)
		return e
	})
	return e
}

// Stable business receipt recovery is metadata only; it grants no asset bytes.
func (s *Store) QueryRehearsalReportCommit(ctx context.Context, scope domain.ProductScope, task string) (CommitReceipt, error) {
	return queryRehearsalReportCommitRow(ctx, s.DB, scope, task)
}
func queryRehearsalReportCommitRow(ctx context.Context, q rowQuerier, scope domain.ProductScope, task string) (CommitReceipt, error) {
	var storage, hash, key, asset string
	var payload []byte
	e := q.QueryRowContext(ctx, "SELECT r.storage_key,r.body_hash,r.receipt_key,r.asset_id,r.commit_hash FROM harness_rehearsal_reports r JOIN harness_admin_rehearsals a USING(tenant_id,product_id,task_id) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND a.owner_subject_id=? AND r.state='COMMITTED'", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&storage, &hash, &key, &asset, &payload)
	if e != nil {
		return CommitReceipt{}, e
	}
	if key != turnIdentity(task, "report-commit") || asset != turnIdentity(task, "report-asset") || len(payload) != 32 {
		return CommitReceipt{}, ErrConflict
	}
	receipt, e := queryCommitScopedRow(ctx, q, scope, nil, scope.SubjectID, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	row, e := queryReceipt(ctx, q, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	want := BarrierRequest{ProductScope: scope, OwnerSubjectID: scope.SubjectID, TaskID: task, Operation: "REHEARSAL_REPORT_COMMIT", CommandKey: key, CommitKey: key}
	copy(want.PayloadHash[:], payload)
	if !receipt.Committed || !row.matches(want) {
		return CommitReceipt{}, ErrConflict
	}
	return receipt, nil
}

func (s *Store) CommitRehearsalReport(ctx context.Context, root domain.TaskLease, worker string) (CommitReceipt, error) {
	if root.WorldInstanceID != nil || root.SubjectID != root.OwnerSubjectID || s.RehearsalReportReader == nil {
		return CommitReceipt{}, ErrDenied
	}
	ctx, release, e := s.EnterOperation(ctx)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer release()
	if receipt, e := s.QueryRehearsalReportCommit(ctx, root.ProductScope, root.TaskID); e == nil {
		return receipt, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return CommitReceipt{}, e
	}
	evidence, e := s.RehearsalReportReader.ReadRehearsalReportEvidence(ctx, root, worker)
	if e != nil {
		return CommitReceipt{}, e
	}
	defer evidence.Close()
	r, e := s.planRehearsalReport(ctx, evidence, root, worker)
	if e != nil {
		return CommitReceipt{}, e
	}
	ctx, e = s.RehearsalReportReader.RehearsalReportContext(evidence, root, worker)
	if e != nil {
		return CommitReceipt{}, e
	}
	// Stage's owning short Tx independently proves all sources/outcomes before IO.
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		if e := s.inspectRehearsalReportTx(ctx, port.(*unit), r); e != nil {
			return e
		}
		return errApprovedInspected
	})
	if !errors.Is(e, errApprovedInspected) {
		if e == nil {
			e = ErrDenied
		}
		return CommitReceipt{}, e
	}
	body, e := reportDocument(r)
	if e != nil {
		return CommitReceipt{}, e
	}
	if e = s.RehearsalReportReader.StageRehearsalReport(evidence, root, worker, body); e != nil {
		return CommitReceipt{}, e
	}
	artifact, e := s.RehearsalReportReader.RehearsalReportArtifact(evidence, root, worker)
	if e != nil {
		return CommitReceipt{}, e
	}
	r.rehearsalReport.artifact = artifact
	r.Operation = "REHEARSAL_REPORT_COMMIT"
	r.CommandKey = turnIdentity(root.TaskID, "report-commit")
	r.CommitKey = r.CommandKey
	_, r.PayloadHash, e = harnessPayload(struct {
		Children      []domain.RehearsalChildOutcome
		Storage, Hash string
	}{r.rehearsalReport.expected, artifact.Storage, artifact.Hash})
	if e != nil {
		return CommitReceipt{}, e
	}
	asset, service := turnIdentity(root.TaskID, "report-asset"), turnIdentity(root.TaskID, "service")
	r.Locks = append(r.Locks, LockKey{Rank: 90, Kind: "asset", IDs: []string{root.TenantID, root.ProductID, asset}}, LockKey{Rank: 90, Kind: "asset", IDs: []string{root.TenantID, root.ProductID, "staging." + artifact.Storage}}, LockKey{Rank: 110, Kind: "service_events", IDs: []string{root.TenantID, root.ProductID, service}})
	for _, h := range r.rehearsalReport.holds {
		r.Locks = append(r.Locks, LockKey{Rank: 110, Kind: "postings", IDs: []string{root.TenantID, root.ProductID, turnIdentity(root.TaskID, "posting", h.ReservationID)}})
	}
	for _, k := range r.Locks {
		r.rehearsalReport.keys[string(lockBytes(k))] = true
	}
	return s.WithBarrier(ctx, r, func(tx Tx) error {
		u := tx.(*unit)
		if e := s.inspectRehearsalReportTx(ctx, u, r); e != nil {
			return e
		}
		if e := s.checkRehearsalReportStageTx(ctx, u, r); e != nil {
			return e
		}
		if e := settleRehearsalReport(ctx, u, r, service); e != nil {
			return e
		}
		var now time.Time
		if e := u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return e
		}
		record := generated.FileRecord{Scope: generated.ProductScope{TenantID: root.TenantID, ProductID: root.ProductID}, AssetID: generated.Identity(asset), State: "READY", Name: "rehearsal-report.json", MediaType: "text/plain", SizeBytes: generated.ExactUnsigned(strconv.Itoa(len(artifact.Body))), Sha256: artifact.Hash, Purpose: "MEDIA", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "R0 isolated rehearsal report", License: "original governed source restrictions", AllowedUses: []string{}}, AudiencePolicy: "PRODUCT_EDITORS", OwnerSubjectID: generated.Identity(root.SubjectID), CreatedAt: now.UTC().Format(time.RFC3339Nano)}
		if e := typedJSON("FileRecord", record); e != nil {
			return e
		}
		raw, e := json.Marshal(record)
		if e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO assets(tenant_id,product_id,asset_id,owner_subject_id,state,storage_key,document) VALUES(?,?,?,?,'READY',?,?)", root.TenantID, root.ProductID, asset, root.SubjectID, artifact.Storage, raw); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE harness_rehearsal_reports SET state='COMMITTED',asset_id=?,receipt_key=?,commit_hash=? WHERE tenant_id=? AND product_id=? AND task_id=? AND state='STAGED' AND delete_intent=0", asset, r.CommitKey, r.PayloadHash[:], root.TenantID, root.ProductID, root.TaskID); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE harness_admin_rehearsals SET fsm_state='COMMITTED',own_steps=2 WHERE tenant_id=? AND product_id=? AND task_id=? AND own_steps=1 AND fsm_state='ISOLATED'", root.TenantID, root.ProductID, root.TaskID); e != nil {
			return e
		}
		if _, e = u.tx.ExecContext(ctx, "UPDATE harness_envelopes SET state='COMMITTED',steps_used=steps_used+1 WHERE tenant_id=? AND product_id=? AND task_id=? AND state='ACTIVE' AND steps_used<max_steps", root.TenantID, root.ProductID, root.TaskID); e != nil {
			return e
		}
		if e = tx.SetTaskState(ctx, "COMMITTED"); e != nil {
			return e
		}
		r.rehearsalReport.applied = u
		return nil
	})
}

func (s *Store) checkRehearsalReportStageTx(ctx context.Context, u *unit, r BarrierRequest) error {
	if e := checkRehearsalReportStageIdentity(ctx, u); e != nil {
		return e
	}
	a, e := s.RehearsalReportReader.RehearsalReportArtifact(r.rehearsalReport.evidence, r.rehearsalReport.root, r.WorkerID)
	if e != nil {
		return e
	}
	body, e := reportDocument(r)
	if e != nil {
		return e
	}
	hash := sha256.Sum256(body)
	if !bytes.Equal(body, a.Body) || a.Storage != r.rehearsalReport.artifact.Storage || a.Hash != hex.EncodeToString(hash[:]) {
		return ErrConflict
	}
	var valid bool
	_, proof, e := harnessPayload(r.rehearsalReport.expected)
	if e != nil {
		return e
	}
	e = u.tx.QueryRowContext(ctx, "SELECT r.storage_key=? AND r.body_hash=? AND r.evidence_hash=? AND r.delete_intent=0 AND f.state='ACTIVE' AND c.operation='REHEARSAL_REPORT_STAGE' AND c.task_id=r.task_id AND c.phase='COMMIT' FROM harness_rehearsal_reports r JOIN file_staging f USING(tenant_id,product_id,storage_key) JOIN commit_receipts c ON c.commit_key=r.stage_receipt AND c.tenant_id=r.tenant_id AND c.product_id=r.product_id WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=?", a.Storage, a.Hash, proof[:], r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}

func checkRehearsalReportStageIdentity(ctx context.Context, u *unit) error {
	p := u.rehearsalReport
	if p == nil {
		return ErrDenied
	}
	var privacy, view uint64
	var key string
	var current bool
	e := u.tx.QueryRowContext(ctx, "SELECT r.subject_privacy,r.subject_view,r.stage_receipt,r.subject_privacy=g.privacy_epoch AND r.subject_view=g.view_epoch FROM harness_rehearsal_reports r JOIN subject_governance g ON g.tenant_id=r.tenant_id AND g.product_id=r.product_id AND g.subject_id=? WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=?", p.root.SubjectID, p.root.TenantID, p.root.ProductID, p.root.TaskID).Scan(&privacy, &view, &key, &current)
	if e != nil {
		return e
	}
	if !current || key != turnIdentity(p.root.TaskID, "report-stage") {
		return ErrDenied
	}
	want := BarrierRequest{ProductScope: p.root.ProductScope, OwnerSubjectID: p.root.SubjectID, TaskID: p.root.TaskID, Operation: "REHEARSAL_REPORT_STAGE", CommandKey: key, CommitKey: key}
	_, want.PayloadHash, e = harnessPayload(struct {
		Storage, Hash string
		Privacy, View uint64
	}{p.artifact.Storage, p.artifact.Hash, privacy, view})
	if e != nil {
		return e
	}
	saved, e := queryReceipt(ctx, u.tx, key)
	if e != nil {
		return e
	}
	if !saved.matches(want) {
		return ErrDenied
	}
	return nil
}

func reportCosts(p *rehearsalReportInspection) []domain.MockResultMetadata {
	out := []domain.MockResultMetadata{}
	for _, c := range p.expected {
		if c.OriginalDependency != nil {
			out = append(out, *c.OriginalDependency)
		}
		out = append(out, c.Metadata)
	}
	return out
}
func reportHoldState(p *rehearsalReportInspection, h reportHold) string {
	allFinal := true
	final := false
	for _, m := range reportCosts(p) {
		known := m.UsageKnown && m.NoFurtherCost
		allFinal = allFinal && known
		if h.task == m.Identity.TaskID && h.purpose == m.Identity.AttemptID {
			final = known
		}
	}
	if h.Kind == "platform_risk" {
		if allFinal {
			return "RELEASED"
		}
		return "HELD"
	}
	if h.task == p.root.TaskID || final {
		if h.task == p.root.TaskID && !allOriginalUsageKnown(reportCosts(p)) {
			return "HELD"
		}
		return "SETTLED"
	}
	return "HELD"
}
func settleRehearsalReport(ctx context.Context, u *unit, r BarrierRequest, service string) error {
	p := r.rehearsalReport
	s := r.ProductScope
	costs := reportCosts(p)
	var valid bool
	if e := u.tx.QueryRowContext(ctx, "SELECT e.calls_allocated=? AND e.max_calls=? AND e.execute_by>UTC_TIMESTAMP(6) AND e.settle_by>UTC_TIMESTAMP(6) AND e.state='ACTIVE' AND e.steps_used<e.max_steps AND q.document->'$.ServiceAtoms'=0 AND q.document->'$.SupplierAtoms'=0 FROM harness_envelopes e JOIN harness_quotes q ON q.tenant_id=e.tenant_id AND q.product_id=e.product_id AND q.quote_id=e.quote_id WHERE e.tenant_id=? AND e.product_id=? AND e.task_id=?", len(costs), 2*len(p.children), s.TenantID, s.ProductID, r.TaskID).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	if e := bindOriginalServiceMeteringTx(ctx, u, r, service, costs); e != nil {
		return e
	}
	for _, h := range p.holds {
		state := reportHoldState(p, h)
		if state == "HELD" {
			continue
		}
		if _, e := u.tx.ExecContext(ctx, "UPDATE reservations SET status=?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=? AND reservation_id=?", state, h.Kind, h.ScopeKey, h.AccountID, h.ReservationID); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "UPDATE budget_accounts SET held=held-?,revision=revision+1 WHERE kind=? AND scope_key=? AND account_id=?", h.Amount, h.Kind, h.ScopeKey, h.AccountID); e != nil {
			return e
		}
		if _, e := u.tx.ExecContext(ctx, "INSERT INTO postings(tenant_id,product_id,record_id,task_id,owner_subject_id,kind,scope_key,account_id,amount,status,revision) VALUES(?,?,?,?,?,?,?,?,0,?,1)", s.TenantID, s.ProductID, turnIdentity(r.TaskID, "posting", h.ReservationID), h.task, s.SubjectID, h.Kind, h.ScopeKey, h.AccountID, state); e != nil {
			return e
		}
	}
	return nil
}
