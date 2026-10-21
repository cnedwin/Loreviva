package mysql

import (
	"bytes"
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"time"
)

// This read plan contains current identity only. It neither grants a new TURN
// nor treats a historical positive-use fact as permission to read a body.
func (s *Store) inspectIntegrationTurnRead(ctx context.Context, q creationRows, v currentIntegration, world string) (*integrationTurnPlan, error) {
	a := &integrationTurnPlan{store: s, principal: v}
	var e error
	a.session, a.eligibility, e = s.currentClient(ctx, q, v)
	if e != nil || a.session.World != world {
		return nil, ErrDenied
	}
	allowed := false
	for _, action := range a.session.Actions {
		allowed = allowed || action == "READ"
	}
	if !allowed {
		return nil, ErrDenied
	}
	a.service, e = s.integrationServiceProof(ctx, q, a.session.Service)
	if e != nil {
		return nil, e
	}
	a.scope = domain.Scope{ProductScope: domain.ProductScope{TenantID: a.session.Tenant, ProductID: a.session.Product, SubjectID: a.session.Player}, WorldInstanceID: world}
	e = q.QueryRowContext(ctx, "SELECT account_id FROM integration_customers WHERE tenant_id=? AND product_id=? AND customer_id=?", a.session.Tenant, a.session.Product, a.session.Customer).Scan(&a.account)
	if e != nil || a.account == "" {
		return nil, ErrDenied
	}
	a.keys = consentKeys(a.session, a.eligibility, "")
	return a, nil
}

// Only the exact original intake and its committed sidecar are projected. The
// final current check runs in the same owning read transaction before return.
func (s *Store) ReadIntegrationIntakeReceipt(ctx context.Context, world, key string) (generated.OriginalTurnIntakeReceipt, error) {
	return s.readIntegrationIntakeReceipt(ctx, world, key, nil)
}
func (s *Store) readIntegrationIntakeReceipt(ctx context.Context, world, key string, request *generated.TurnRequest) (generated.OriginalTurnIntakeReceipt, error) {
	var out generated.OriginalTurnIntakeReceipt
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || domain.ValidateID(world) != nil || domain.ValidateIdempotencyKey(key) != nil {
		return out, ErrDenied
	}
	a, e := s.inspectIntegrationTurnRead(ctx, s.DB, v, world)
	if e != nil {
		return out, e
	}
	a.key = key
	a.task = turnIdentity(a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, world, "TURN", key)
	keys := append([]LockKey{}, a.keys...)
	keys = append(keys, LockKey{Rank: 40, IDs: []string{a.scope.TenantID, a.scope.ProductID, world}}, harnessTaskKey(a.scope.ProductScope, a.task), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(a.task, "accepted")}})
	tx, e := s.integrationMemoryReadTx(ctx, keys)
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	check := func() error {
		n, e := s.inspectIntegrationTurnRead(ctx, tx, v, world)
		if e != nil || !reflect.DeepEqual(a.session, n.session) || !reflect.DeepEqual(a.eligibility, n.eligibility) || !reflect.DeepEqual(a.service, n.service) || a.account != n.account {
			return ErrDenied
		}
		var hash []byte
		var quote string
		e = tx.QueryRowContext(ctx, `SELECT h.command_hash,h.quote_id,i.accepted_use_generation,i.accepted_consent_revision,i.accepted_use_expires_at FROM harness_turns h JOIN integration_task_principals i USING(tenant_id,product_id,task_id) WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=? AND h.owner_subject_id=? AND h.world_instance_id=? AND h.original_key=?`, a.scope.TenantID, a.scope.ProductID, a.task, a.scope.SubjectID, world, key).Scan(&hash, &quote, &n.use.Generation, &n.use.Revision, &n.use.Expires)
		if e != nil {
			return e
		}
		if len(hash) != 32 {
			return ErrDenied
		}
		if a.quote != "" && (a.quote != quote || !bytes.Equal(a.hash[:], hash) || a.use != n.use) {
			return ErrDenied
		}
		a.quote = quote
		a.use = n.use
		copy(a.hash[:], hash)
		if request != nil {
			expected, err := TurnCommandHash(a.scope, *request)
			if err != nil {
				return err
			}
			if expected != a.hash {
				return ErrConflict
			}
		}
		return a.verifyIntake(ctx, tx, a.intakeRequest(), a.task, true, true)
	}
	if e = check(); e != nil {
		return out, e
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return out, e
		}
	}
	if e = check(); e != nil {
		return out, e
	}
	out = generated.OriginalTurnIntakeReceipt{Scope: turnScope(a.scope), CustomerID: generated.Identity(a.session.Customer), CallerSubjectID: generated.Identity(a.session.Player), OperationID: "submitTurn", OriginalKey: key, IntakeReceiptRef: generated.Identity(turnIdentity(a.task, "accepted")), Acceptance: generated.TurnAccepted{TurnID: a.task, QuoteID: a.quote, Task: generated.TaskReceipt{TaskID: a.task, TaskKind: "TURN", TaskState: "ACCEPTED", Scope: turnScope(a.scope), CorrelationID: a.task}}}
	return out, typedJSON("OriginalTurnIntakeReceipt", out)
}

// The fact is checked against its original receipt and original issuing Session;
// neither a newer Session nor a newer choice rewrites that historical identity.
func (a *integrationTurnPlan) verifyAcceptedConsent(ctx context.Context, q creationRows, revision, generation uint64, expiry time.Time) error {
	var key string
	var hash, raw []byte
	var valid bool
	e := q.QueryRowContext(ctx, `SELECT f.original_key,f.payload_hash,x.result_metadata,(f.use_authority_generation=? AND f.expires_at=? AND f.action IN ('INITIAL','RENEW') AND f.policy=? AND x.payload_hash=f.payload_hash AND x.receipt_ref=f.receipt_ref AND x.target_revision=f.revision AND x.state='COMMITTED' AND x.committed_at=f.recorded_at) FROM integration_self_consent_facts f JOIN integration_receipts x ON x.tenant_id=f.tenant_id AND x.product_id=f.product_id AND x.owner_id=f.player_subject_id AND x.original_key=f.original_key AND x.operation='PLAYER_SELF_CONSENT' AND x.scope_kind='PRODUCT' AND x.scope_address=f.product_id WHERE f.tenant_id=? AND f.product_id=? AND f.player_subject_id=? AND f.revision=?`, generation, expiry, playerConsentPolicy, a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, revision).Scan(&key, &hash, &raw, &valid)
	if e != nil || !valid || len(hash) != 32 {
		return ErrDenied
	}
	var receipt generated.PlayerSelfConsentReceipt
	if json.Unmarshal(raw, &receipt) != nil {
		return ErrDenied
	}
	current := a.session
	if current.ID == "" {
		current = sessionRecord{Tenant: a.scope.TenantID, Product: a.scope.ProductID, Player: a.scope.SubjectID, Installation: a.eligibility.identity.Installation}
	}
	original, e := consentOriginalSession(ctx, q, current, key)
	if e != nil {
		return e
	}
	var h [32]byte
	copy(h[:], hash)
	return validatePlayerConsentReceipt(ctx, q, original, key, h, receipt)
}
