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
	"loreviva.local/loreviva/internal/wire"
	"reflect"
	"strconv"
	"time"
)

const withdrawalOperation = "withdrawContentRelease"

// These seals are minted only from the real Operator and original publication.
// Neither a copied receipt nor a caller-supplied Tx can construct a write proof.
type contentWithdrawalPlan struct {
	store                          *Store
	operator                       currentOperator
	scope                          domain.ProductScope
	release, expected, key, commit string
	hash                           [32]byte
	proof                          withdrawalPublication
	keys                           []LockKey
	unit                           *unit
	write                          *contentWithdrawalWrite
}
type contentWithdrawalWrite struct {
	plan                            *contentWithdrawalPlan
	unit                            *unit
	tx                              *sql.Tx
	operation, key, commit, release string
	scope                           domain.ProductScope
	hash                            [32]byte
	expectedAvailable               bool
}
type withdrawalPublication struct {
	draft                                         string
	revision                                      uint64
	hash, document, resultDocument, checkDocument []byte
	release                                       generated.Release
	result, resultOwner, schema                   string
	checkDraft, checkKind                         string
	checkRevision                                 uint64
	receipt                                       receiptRow
	instance                                      string
	committed                                     time.Time
}
type withdrawalQuerier interface {
	rowQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func withdrawalHash(v any) ([32]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return [32]byte{}, e
	}
	b, e = wire.Canonical(b)
	if e != nil {
		return [32]byte{}, e
	}
	return sha256.Sum256(b), nil
}
func withdrawalHex(v any) (string, error) {
	h, e := withdrawalHash(v)
	return hex.EncodeToString(h[:]), e
}

// Retained lookup is deliberately separate from live CREATE/Memory visibility.
// Availability is classified only after locking; immutable evidence is re-read.
func readWithdrawalPublication(ctx context.Context, q withdrawalQuerier, scope domain.ProductScope, id string) (out withdrawalPublication, live bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT c.draft_id,c.draft_revision,c.content_hash,c.document,r.available FROM content_releases c JOIN releases r USING(tenant_id,product_id,release_id) WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=?`, scope.TenantID, scope.ProductID, id).Scan(&out.draft, &out.revision, &out.hash, &out.document, &live)
	if err != nil {
		return
	}
	if generated.Decode("Release", out.document, &out.release) != nil {
		return out, live, ErrDenied
	}
	r := out.release
	if r.Scope.TenantID != scope.TenantID || r.Scope.ProductID != scope.ProductID || string(r.ReleaseID) != id || string(r.DraftID) != out.draft || string(r.DraftRevision) != strconv.FormatUint(out.revision, 10) || string(r.PublishedBySubjectID) != scope.SubjectID || !r.MockOnly || len(out.hash) != 32 || hex.EncodeToString(out.hash) != r.ContentHash || !reflect.DeepEqual(r.DependencyLocks, r.Input.DependencyLocks) {
		return out, live, ErrDenied
	}
	h, e := withdrawalHex(struct {
		Input        generated.DraftInput
		Dependencies []generated.DependencyLock
		Mappings     any
	}{r.Input, r.DependencyLocks, r.RouteMappings})
	if e != nil || h != r.ContentHash {
		return out, live, ErrDenied
	}
	if _, e = time.Parse(time.RFC3339Nano, r.PublishedAt); e != nil {
		return out, live, ErrDenied
	}
	rows, e := q.QueryContext(ctx, `SELECT result_id,owner_subject_id,schema_name,document FROM admin_results WHERE tenant_id=? AND product_id=? AND schema_name='Release' AND JSON_UNQUOTE(JSON_EXTRACT(CONVERT(document USING utf8mb4),'$.release_id'))=?`, scope.TenantID, scope.ProductID, id)
	if e != nil {
		return out, live, e
	}
	n := 0
	for rows.Next() {
		n++
		if n > 1 {
			rows.Close()
			return out, live, ErrDenied
		}
		if e = rows.Scan(&out.result, &out.resultOwner, &out.schema, &out.resultDocument); e != nil {
			rows.Close()
			return out, live, e
		}
	}
	e = rows.Err()
	closeErr := rows.Close()
	if e != nil {
		return out, live, e
	}
	if closeErr != nil {
		return out, live, closeErr
	}
	if n != 1 || out.resultOwner != scope.SubjectID || out.schema != "Release" || !bytes.Equal(out.resultDocument, out.document) {
		return out, live, ErrDenied
	}
	out.receipt, e = queryReceipt(ctx, q, out.result)
	if e != nil {
		return out, live, e
	}
	c := out.receipt
	if c.key != out.result || c.tenant != scope.TenantID || c.product != scope.ProductID || c.owner != scope.SubjectID || c.kind != "PRODUCT" || c.world.Valid || c.nullTask.Valid || c.operation != "publishContentDraft" || c.phase != "COMMIT" || len(c.hash) != 32 || domain.ValidateIdempotencyKey(c.command) != nil || out.result != creationID(scope.TenantID, scope.ProductID, scope.SubjectID, "publishContentDraft", c.command) || id != creationID(scope.TenantID, scope.ProductID, scope.SubjectID, "publish", c.command) {
		return out, live, ErrDenied
	}
	if e = q.QueryRowContext(ctx, "SELECT instance_scope,committed_at FROM commit_receipts WHERE commit_key=?", out.result).Scan(&out.instance, &out.committed); e != nil || out.instance != "" || out.committed.IsZero() {
		return out, live, ErrDenied
	}
	e = q.QueryRowContext(ctx, `SELECT draft_id,draft_revision,report_kind,document FROM content_checks WHERE tenant_id=? AND product_id=? AND report_id=?`, scope.TenantID, scope.ProductID, r.CheckReportID).Scan(&out.checkDraft, &out.checkRevision, &out.checkKind, &out.checkDocument)
	if e != nil {
		return out, live, e
	}
	var check generated.PublishCheckReport
	if generated.Decode("PublishCheckReport", out.checkDocument, &check) != nil || out.checkDraft != out.draft || out.checkRevision != out.revision || out.checkKind != "PUBLISH" || check.Scope != r.Scope || check.ReportID != r.CheckReportID || check.DraftID != r.DraftID || check.DraftRevision != r.DraftRevision || !check.Passed || len(check.Findings) != 0 || !check.MockOnly || !reflect.DeepEqual(check.RegisteredCapabilityIDS, []generated.Identity{"TEXT", "SIGN_CUSTODY"}) {
		return out, live, ErrDenied
	}
	inputHash, e := withdrawalHex(r.Input)
	if e != nil || inputHash != check.ContentHash {
		return out, live, ErrDenied
	}
	routeHash, e := withdrawalHex(struct {
		Mappings     any
		Capabilities []string
		Input        generated.DraftInput
	}{r.RouteMappings, []string{"TEXT", "SIGN_CUSTODY"}, r.Input})
	if e != nil || routeHash != check.RouteSnapshotHash {
		return out, live, ErrDenied
	}
	// This is historical issuance, not a new check. Keep both original label
	// arrays and raw documents; do not invent a current-expiry or request-preimage gate.
	if _, e = time.Parse(time.RFC3339Nano, check.ExpiresAt); e != nil {
		return out, live, ErrDenied
	}
	return out, live, nil
}

func (s *Store) WithdrawContentRelease(ctx context.Context, scope domain.ProductScope, id, expected, key string) (CommitReceipt, error) {
	if scope.Validate() != nil || domain.ValidateID(id) != nil || domain.ValidateIdempotencyKey(key) != nil {
		return CommitReceipt{}, ErrDenied
	}
	h, e := hex.DecodeString(expected)
	if e != nil || len(h) != 32 || hex.EncodeToString(h) != expected {
		return CommitReceipt{}, ErrDenied
	}
	op, e := currentOperatorTx(ctx, s.DB, s, scope)
	if e != nil || op.job != "" || op.worker != "" || op.family != "" {
		return CommitReceipt{}, ErrDenied
	}
	if e = operatorActionTx(ctx, s.DB, s, scope, "content.archive"); e != nil {
		return CommitReceipt{}, e
	}
	proof, _, e := readWithdrawalPublication(ctx, s.DB, scope, id)
	if e != nil {
		return CommitReceipt{}, e
	}
	// An incorrect expected hash is a request conflict, never a normalized alias.
	if proof.release.ContentHash != expected {
		return CommitReceipt{}, ErrConflict
	}
	hash, e := withdrawalHash(struct {
		Scope             domain.ProductScope
		Operation, Target string
		Input             any
	}{scope, withdrawalOperation, id, struct{ ExpectedContentHash, Action string }{expected, "WITHDRAW"}})
	if e != nil {
		return CommitReceipt{}, e
	}
	a := &contentWithdrawalPlan{store: s, operator: op, scope: scope, release: id, expected: expected, key: key, commit: creationID(scope.TenantID, scope.ProductID, scope.SubjectID, withdrawalOperation, key), hash: hash, proof: proof}
	defer func() { a.unit = nil; a.write = nil }()
	a.keys = []LockKey{{Rank: 90, Kind: "content_release", IDs: []string{scope.TenantID, scope.ProductID, id}}, {Rank: 90, Kind: "content_check", IDs: []string{scope.TenantID, scope.ProductID, proof.result}}, {Rank: 90, Kind: "content_check", IDs: []string{scope.TenantID, scope.ProductID, string(proof.release.CheckReportID)}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{proof.result}}}
	var gates GateVersions
	var enabled bool
	e = s.DB.QueryRowContext(ctx, `SELECT t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,t.enabled AND p.enabled AND g.enabled AND g.can_execute FROM subject_governance g JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=?`, scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled)
	if e != nil || !enabled {
		return CommitReceipt{}, ErrDenied
	}
	r := CommandBarrierRequest{contentWithdrawal: a, ProductScope: scope, OwnerSubjectID: scope.SubjectID, Operation: withdrawalOperation, CommandKey: key, CommitKey: a.commit, PayloadHash: hash, GateVersions: gates, Locks: a.keys}
	return s.WithCommandBarrier(ctx, r, func(tx Tx) error {
		u, ok := tx.(*unit)
		if !ok || u != a.unit || u.contentWithdrawal != a || u.tx == nil {
			return ErrDenied
		}
		if e := u.RequireLock(LockKey{Rank: 90, Kind: "content_release", IDs: []string{scope.TenantID, scope.ProductID, id}}); e != nil {
			return e
		}
		result, e := u.ExecContext(ctx, `UPDATE releases SET available=0 WHERE tenant_id=? AND product_id=? AND release_id=? AND available=1`, scope.TenantID, scope.ProductID, id)
		if e != nil {
			return e
		}
		if e = registryAffected(result); e != nil {
			return e
		}
		a.write = &contentWithdrawalWrite{plan: a, unit: u, tx: u.tx, scope: scope, operation: withdrawalOperation, key: key, commit: a.commit, release: id, hash: hash, expectedAvailable: false}
		return nil
	})
}

// Operation/seal correspondence is enforced before any unrelated early return.
func (s *Store) prepareContentWithdrawal(ctx context.Context, r *BarrierRequest) error {
	a := r.contentWithdrawal
	if a == nil {
		if r.Operation == withdrawalOperation {
			return ErrDenied
		}
		return nil
	}
	if a.store != s || a.unit != nil || a.write != nil || r.Operation != withdrawalOperation || r.ProductScope != a.scope || r.OwnerSubjectID != a.scope.SubjectID || r.CommandKey != a.key || r.CommitKey != a.commit || r.PayloadHash != a.hash || !r.commandOnly || r.WorldScope != nil || r.TaskID != "" || r.WorkerID != "" || r.Runtime != nil || r.Purpose != ExecutionPurpose || r.integrationTurn != nil || r.integrationControl != nil || r.integrationCreation != nil || r.candidateExecution || r.candidateIntake != nil || r.candidateHistoryRead != nil || r.rehearsalInitialization != nil || r.rehearsalExecution != nil || r.rehearsalHistory != nil || r.contentAction != "" {
		return ErrDenied
	}
	op, e := currentOperatorTx(ctx, s.DB, s, r.ProductScope)
	if e != nil || op != a.operator || op.job != "" || op.worker != "" || op.family != "" {
		return ErrDenied
	}
	return nil
}

func (s *Store) checkContentWithdrawal(ctx context.Context, u *unit, r BarrierRequest, final bool) error {
	a := r.contentWithdrawal
	if a == nil {
		if r.Operation == withdrawalOperation {
			return ErrDenied
		}
		return nil
	}
	if a.store != s || u == nil || a.unit != u || u.contentWithdrawal != a || u.operatorStore != s || u.tx == nil || !u.commandOnly || u.taskID != "" || u.world != "" || u.scope != a.scope || u.operation != withdrawalOperation || r.Operation != withdrawalOperation || r.ProductScope != a.scope || r.OwnerSubjectID != a.scope.SubjectID || r.CommandKey != a.key || r.CommitKey != a.commit || r.PayloadHash != a.hash {
		return ErrDenied
	}
	for _, k := range append(baseLocks(r), a.keys...) {
		if e := u.RequireLock(k); e != nil {
			return e
		}
	}
	op, e := currentOperatorTx(ctx, u.tx, s, a.scope)
	if e != nil || op != a.operator {
		return ErrDenied
	}
	if e = operatorActionTx(ctx, u.tx, s, a.scope, "content.archive"); e != nil {
		return e
	}
	proof, live, e := readWithdrawalPublication(ctx, u.tx, a.scope, a.release)
	if e != nil || !reflect.DeepEqual(proof, a.proof) || proof.release.ContentHash != a.expected {
		return ErrDenied
	}
	original, e := queryReceipt(ctx, u.tx, a.commit)
	if e == nil {
		if original.key != a.commit || original.nullTask.Valid || original.world.Valid || !original.matches(r) {
			return ErrConflict
		}
		var instance string
		if e = u.tx.QueryRowContext(ctx, "SELECT instance_scope FROM commit_receipts WHERE commit_key=?", a.commit).Scan(&instance); e != nil || instance != "" {
			return ErrDenied
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if final {
		w := a.write
		if e != nil || w == nil || w.plan != a || w.unit != u || w.tx != u.tx || w.scope != a.scope || w.operation != r.Operation || w.key != r.CommandKey || w.commit != r.CommitKey || w.hash != r.PayloadHash || w.release != a.release || w.expectedAvailable || live != w.expectedAvailable {
			return ErrDenied
		}
	} else if e == nil {
		if live || a.write != nil {
			return ErrDenied
		}
	} else if !live || a.write != nil {
		return ErrDenied
	}
	return nil
}
