package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"time"
)

type integrationTurnKey struct{}
type integrationUse struct {
	Generation, Revision uint64
	Expires              time.Time
}
type integrationTurnPlan struct {
	choice                            *integrationChoice
	fresh                             *integrationFreshWrite
	store                             *Store
	control                           *integrationControlPlan
	principal, service                currentIntegration
	session                           sessionRecord
	eligibility                       sessionEligibility
	scope                             domain.Scope
	account, action, key, quote, task string
	hash                              [32]byte
	keys                              []LockKey
	use                               integrationUse
	output                            generated.Quote
	lease                             *domain.TaskLease
	worker                            string
}

// A current choice is distinct from an accepted historical use. Its addresses
// are resolved before the transaction and may never select new locks inside it.
type integrationChoice struct {
	Use                             integrationUse
	Key, Session, Service, Exchange string
}

func (a *integrationTurnPlan) inspectCurrentChoice(ctx context.Context, q creationRows, bound *integrationChoice) (*integrationChoice, error) {
	use, e := integrationCurrentTurnUse(ctx, q, a.scope)
	if e != nil {
		return nil, e
	}
	p := &integrationChoice{Use: use}
	e = q.QueryRowContext(ctx, `SELECT f.original_key,f.principal_ref,k.issuer_service_key,x.original_key FROM integration_self_consent_facts f JOIN integration_principals k ON k.principal_id=f.principal_ref AND k.kind='CLIENT' AND k.tenant_id=f.tenant_id AND k.product_id=f.product_id JOIN integration_receipts x ON x.tenant_id=k.tenant_id AND x.product_id=k.product_id AND x.owner_id=k.issuer_service_key AND x.receipt_ref=k.issuance_receipt AND x.operation='EXCHANGE_SESSION' WHERE f.tenant_id=? AND f.product_id=? AND f.player_subject_id=? AND f.revision=?`, a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, use.Revision).Scan(&p.Key, &p.Session, &p.Service, &p.Exchange)
	if e != nil || p.Key == "" || p.Session == "" || p.Service == "" || p.Exchange == "" || (bound != nil && *p != *bound) {
		return nil, ErrDenied
	}
	proof := *a
	if proof.session.ID == "" {
		// SERVICE has no CLIENT Session. Resolve only the precollected original
		// issuance as historical evidence; it never becomes a current principal.
		service := a.service.principal
		service.PrincipalID = p.Service
		original, err := sessionRead(ctx, q, service, p.Exchange)
		if err != nil || original.ID != p.Session || original.Tenant != a.scope.TenantID || original.Product != a.scope.ProductID || original.Player != a.scope.SubjectID || original.Installation != a.eligibility.identity.Installation {
			return nil, ErrDenied
		}
		proof.session = original
	}
	if e = proof.verifyAcceptedConsent(ctx, q, use.Revision, use.Generation, use.Expires); e != nil {
		return nil, e
	}
	return p, nil
}
func (a *integrationTurnPlan) choiceLocks() []LockKey {
	p := a.choice
	if p == nil {
		return nil
	}
	return []LockKey{
		{Rank: 15, Kind: "integration_principal", IDs: []string{p.Session}},
		{Rank: 15, Kind: "integration_principal", IDs: []string{p.Service}},
		{Rank: 110, Kind: "integration_receipt", IDs: []string{a.scope.TenantID, a.scope.ProductID, p.Service, p.Exchange}},
		{Rank: 110, Kind: "integration_receipt", IDs: []string{a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, p.Key}},
	}
}

// This value is frozen only by a successful private sidecar writer. A pointer
// alone grants nothing: final validation binds Store, plan, transaction, request
// and every persisted value to those actual write arguments. Replays have none.
type integrationFreshWrite struct {
	store                                                   *Store
	unit                                                    *unit
	tx                                                      *sql.Tx
	turn                                                    *integrationTurnPlan
	control                                                 *integrationControlPlan
	scope                                                   domain.Scope
	task, unitTask, operation, command, receipt             string
	kind, principal, ref, service, customer, player, action string
	hash                                                    [32]byte
	registration                                            integrationRegistrationProof
	accepted, issued, execute, settle                       time.Time
	use                                                     integrationUse
}

func (f *integrationFreshWrite) check(ctx context.Context, q creationRows, r BarrierRequest) error {
	tx, ok := q.(*sql.Tx)
	if f == nil || !ok || tx != f.tx || f.unit == nil || f.unit.tx != tx || f.unit.operatorStore != f.store || f.store.integrationClosed.Load() || !f.unit.commandOnly || !r.commandOnly || f.unit.operation != f.operation || r.Operation != f.operation || f.unit.taskID != f.unitTask || r.TaskID != f.unitTask || f.unit.scope != f.scope.ProductScope || r.ProductScope != f.scope.ProductScope || f.unit.world != f.scope.WorldInstanceID || r.WorldScope == nil || *r.WorldScope != f.scope || r.OwnerSubjectID != f.scope.SubjectID || r.CommandKey != f.command || r.CommitKey != f.receipt || r.PayloadHash != f.hash {
		return ErrDenied
	}
	if f.turn != nil {
		a := f.turn
		if f.control != nil || a.fresh != f || a.store != f.store || f.unit.integrationTurn != a || r.integrationTurn != a || a.action != "TURN" || a.lease != nil || f.task != turnIdentity(f.scope.TenantID, f.scope.ProductID, f.scope.SubjectID, f.scope.WorldInstanceID, "TURN", a.key) {
			return ErrDenied
		}
	} else {
		a := f.control
		if a == nil || a.fresh != f || a.store != f.store || f.unit.integrationControl != a || r.integrationControl != a || a.lease != nil || a.document.Task != f.task || a.document.Action != f.action {
			return ErrDenied
		}
	}
	var gen, rev, expiry any
	if f.kind == "TURN" {
		gen = f.use.Generation
		rev = f.use.Revision
		expiry = f.use.Expires
	}
	var valid bool
	var reg []byte
	e := q.QueryRowContext(ctx, `SELECT (task_kind=? AND principal_kind=? AND principal_ref=? AND issuer_service_key=? AND customer_id=? AND player_subject_id=? AND world_instance_id=? AND action=? AND accepted_at=? AND issued_session_expires_at <=> ? AND original_execute_by=? AND original_settle_by=? AND intake_receipt=? AND intake_hash=? AND accepted_use_generation <=> ? AND accepted_use_expires_at <=> ? AND accepted_consent_revision <=> ?),registration_revisions FROM integration_task_principals WHERE tenant_id=? AND product_id=? AND task_id=?`, f.kind, f.principal, f.ref, f.service, f.customer, f.player, f.scope.WorldInstanceID, f.action, f.accepted, f.issued, f.execute, f.settle, f.receipt, f.hash[:], gen, expiry, rev, f.scope.TenantID, f.scope.ProductID, f.task).Scan(&valid, &reg)
	var registered integrationRegistrationProof
	d := json.NewDecoder(bytes.NewReader(reg))
	d.DisallowUnknownFields()
	if e != nil || !valid || d.Decode(&registered) != nil || registered != f.registration {
		return ErrDenied
	}
	return nil
}

// The plan is created only from the Store's authenticated principal. Its player
// scope is a derived address, never a caller-supplied authority substitute.
func (s *Store) inspectIntegrationTurn(ctx context.Context, q creationRows, v currentIntegration, world string, background bool, bound ...*integrationChoice) (*integrationTurnPlan, error) {
	a := &integrationTurnPlan{store: s, principal: v}
	if v.store != s || s.integrationClosed.Load() || s.Guard == nil || ctx.Value(operatorContextKey{}) != nil {
		return nil, ErrDenied
	}
	var e error
	if v.principal.Kind == domain.IntegrationClient {
		a.session, a.eligibility, e = s.currentClientForTask(ctx, q, v, background)
		if e != nil || a.session.World != world {
			return nil, ErrDenied
		}
		found := false
		for _, x := range a.session.Actions {
			if x == "TURN" {
				found = true
			}
		}
		if !found {
			return nil, ErrDenied
		}
		a.service, e = s.integrationServiceProof(ctx, q, a.session.Service)
	} else if v.principal.Kind == domain.IntegrationService {
		a.service = v
		var external, entitlement string
		e = q.QueryRowContext(ctx, "SELECT external_id,entitlement_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND customer_id=? AND service_owner_id=?", v.principal.Tenant, v.principal.Product, world, v.principal.Customer, v.principal.StableSubject).Scan(&external, &entitlement)
		if e == nil {
			a.eligibility, e = s.sessionEligibility(ctx, q, v, generated.ClientSessionRequest{ProductID: v.principal.Product, WorldInstanceID: world, ExternalSubjectID: external, EntitlementReference: entitlement})
		}
	} else {
		return nil, ErrDenied
	}
	if e != nil {
		return nil, e
	}
	var actions []string
	if json.Unmarshal([]byte(a.eligibility.entitlement.Actions), &actions) != nil {
		return nil, ErrDenied
	}
	allowed := false
	for _, x := range actions {
		if x == "TURN" {
			allowed = true
		}
	}
	if !allowed {
		return nil, ErrDenied
	}
	p := a.service.principal
	a.scope = domain.Scope{ProductScope: domain.ProductScope{TenantID: p.Tenant, ProductID: p.Product, SubjectID: a.eligibility.entitlement.Player}, WorldInstanceID: world}
	if e = q.QueryRowContext(ctx, "SELECT account_id FROM integration_customers WHERE tenant_id=? AND product_id=? AND customer_id=?", p.Tenant, p.Product, p.Customer).Scan(&a.account); e != nil || a.account == "" {
		return nil, ErrDenied
	}
	a.keys = []LockKey{{Rank: 15, Kind: "integration_principal", IDs: []string{p.PrincipalID}}, {Rank: 25, Kind: "integration_entitlement", IDs: []string{p.Tenant, p.Product, p.Customer, a.eligibility.entitlement.ID}}, {Rank: 30, IDs: []string{p.Tenant, p.Product, p.StableSubject}}, {Rank: 30, IDs: []string{p.Tenant, p.Product, a.scope.SubjectID}}, {Rank: 35, Kind: "integration_customer", IDs: []string{p.Tenant, p.Product, p.Customer}}, {Rank: 35, Kind: "integration_mapping", IDs: []string{p.Tenant, p.Product, p.Customer, creationExternalHash([]byte(a.eligibility.mapping.External))}}, {Rank: 35, Kind: "integration_participant", IDs: []string{p.Tenant, p.Product, world}}, {Rank: 90, Kind: "integration_consent", IDs: []string{p.Tenant, p.Product, a.scope.SubjectID}}}
	if a.session.ID != "" {
		a.keys = append(a.keys, LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{a.session.ID}}, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{p.Tenant, p.Product, p.PrincipalID, a.session.Key}})
	}
	a.keys = append(a.keys, a.eligibility.keys...)

	if len(bound) > 1 {
		return nil, ErrDenied
	}
	var expected *integrationChoice
	if len(bound) == 1 {
		expected = bound[0]
		if expected == nil {
			return nil, ErrDenied
		}
	}
	a.choice, e = a.inspectCurrentChoice(ctx, q, expected)
	if e != nil {
		return nil, e
	}
	a.use = a.choice.Use
	a.keys = append(a.keys, a.choiceLocks()...)
	return a, nil
}

func integrationCurrentTurnUse(ctx context.Context, q creationRows, scope domain.Scope) (integrationUse, error) {
	var out integrationUse
	var valid bool
	e := q.QueryRowContext(ctx, `SELECT c.use_authority_generation,c.revision,c.expires_at,(c.policy=? AND c.action IN ('INITIAL','RENEW') AND c.expires_at>UTC_TIMESTAMP(6) AND c.expires_at=f.expires_at AND c.accepted_at=f.accepted_at AND c.action=f.action AND c.policy=f.policy AND c.use_authority_generation=f.use_authority_generation AND c.receipt_ref=f.receipt_ref AND f.payload_hash=x.payload_hash AND x.target_revision=c.revision AND x.state='COMMITTED' AND x.operation='PLAYER_SELF_CONSENT' AND x.scope_kind='PRODUCT' AND x.scope_address=c.product_id AND p.enabled=1 AND p.expires_at=c.expires_at) FROM integration_self_consent_current c JOIN integration_self_consent_facts f ON f.tenant_id=c.tenant_id AND f.product_id=c.product_id AND f.player_subject_id=c.player_subject_id AND f.revision=c.revision JOIN integration_receipts x ON x.tenant_id=f.tenant_id AND x.product_id=f.product_id AND x.owner_id=f.player_subject_id AND x.original_key=f.original_key AND x.receipt_ref=f.receipt_ref JOIN memory_subject_permissions p ON p.tenant_id=c.tenant_id AND p.product_id=c.product_id AND p.caller_subject_id=c.player_subject_id AND p.data_subject_id=c.player_subject_id AND p.action='memory.generation.use' AND p.allowed_use='PRIVATE_RESPONSE' WHERE c.tenant_id=? AND c.product_id=? AND c.player_subject_id=?`, playerConsentPolicy, scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&out.Generation, &out.Revision, &out.Expires, &valid)
	if e != nil || !valid || out.Generation == 0 || out.Revision == 0 {
		return out, ErrDenied
	}
	return out, nil
}

func (s *Store) integrationTurnCommand(ctx context.Context, a *integrationTurnPlan, key string) (CommandBarrierRequest, error) {
	var v domain.Versions
	var g GateVersions
	var enabled bool
	p := a.scope.ProductScope
	e := s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,t.enabled AND p.enabled AND g.enabled AND g.can_execute FROM subject_governance g JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=?", p.TenantID, p.ProductID, p.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &enabled)
	if e != nil || !enabled {
		return CommandBarrierRequest{}, ErrDenied
	}
	e = s.DB.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND status='RUNNING'", p.TenantID, p.ProductID, a.scope.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch)
	if e != nil {
		return CommandBarrierRequest{}, e
	}
	return CommandBarrierRequest{integrationTurn: a, ProductScope: p, WorldScope: &a.scope, Versions: &v, Purpose: ExecutionPurpose, OwnerSubjectID: p.SubjectID, CommandKey: key, GateVersions: g, Locks: append([]LockKey{}, a.keys...)}, nil
}

func (s *Store) QuoteIntegrationTurn(ctx context.Context, key string, request generated.QuoteRequest) (generated.Quote, error) {
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return generated.Quote{}, e
	}
	defer done()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || v.principal.Kind != domain.IntegrationService || request.Scope.TenantID != v.principal.Tenant || request.Scope.ProductID != v.principal.Product || domain.ValidateIdempotencyKey(key) != nil {
		return generated.Quote{}, ErrDenied
	}
	a, e := s.inspectIntegrationTurn(ctx, s.DB, v, request.Scope.WorldInstanceID, false)
	if e != nil {
		return generated.Quote{}, fmt.Errorf("integration quote identity/use: %w", e)
	}
	a.action = "QUOTE"
	a.key = key
	c, e := s.integrationTurnCommand(ctx, a, key)
	if e != nil {
		return generated.Quote{}, fmt.Errorf("integration quote command: %w", e)
	}
	out, err := s.QuoteTurn(context.WithValue(ctx, integrationTurnKey{}, a), c, request)
	if err != nil {
		return out, fmt.Errorf("integration quote owning: %w", err)
	}
	return out, nil
}

func (s *Store) AcceptIntegrationTurn(ctx context.Context, world, key string, request generated.TurnRequest) (generated.TurnAccepted, error) {
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	defer done()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || v.principal.Kind != domain.IntegrationClient || domain.ValidateIdempotencyKey(key) != nil {
		return generated.TurnAccepted{}, ErrDenied
	}
	if original, err := s.integrationReplayAccepted(ctx, world, key, request); err == nil {
		return original, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return generated.TurnAccepted{}, err
	}
	a, e := s.inspectIntegrationTurn(ctx, s.DB, v, world, false)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	a.action = "TURN"
	a.key = key
	a.quote = request.QuoteID
	c, e := s.integrationTurnCommand(ctx, a, key)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	return s.AcceptR0Turn(context.WithValue(ctx, integrationTurnKey{}, a), c, request)
}

// Precollection returns only original initialization needed to plan exact locks.
// The live private plan must be present; its bytes grant no runtime authority.
func (s *Store) IntegrationTurnPrecollection(ctx context.Context, scope domain.Scope) ([]byte, bool, error) {
	a, ok := ctx.Value(integrationTurnKey{}).(*integrationTurnPlan)
	if !ok {
		return nil, false, nil
	}
	if a.store != s || a.scope != scope || s.integrationClosed.Load() {
		return nil, true, ErrDenied
	}
	return append([]byte{}, a.eligibility.creation.initial...), true, nil
}

// IntegrationTurnInitializationTx accepts only the concrete Store-issued unit,
// after the closed plan's current check and precise world/subject locks.
func IntegrationTurnInitializationTx(ctx context.Context, tx Tx, scope domain.Scope) ([]byte, bool, error) {
	u, ok := tx.(*unit)
	if !ok || u.integrationTurn == nil {
		return nil, false, nil
	}
	a := u.integrationTurn
	if a.store != u.operatorStore || a.scope != scope || u.scope != scope.ProductScope || u.world != scope.WorldInstanceID || u.err != nil || a.store.integrationClosed.Load() {
		return nil, true, ErrDenied
	}
	for _, k := range []LockKey{{Rank: 30, IDs: []string{scope.TenantID, scope.ProductID, scope.SubjectID}}, {Rank: 40, IDs: []string{scope.TenantID, scope.ProductID, scope.WorldInstanceID}}} {
		if e := u.RequireLock(k); e != nil {
			return nil, true, e
		}
	}
	return append([]byte{}, a.eligibility.creation.initial...), true, nil
}

func (s *Store) checkIntegrationTurn(ctx context.Context, q creationRows, r BarrierRequest) error {
	a := r.integrationTurn
	if a == nil {
		return nil
	}
	if a.action == "APPROVED_READ" {
		return s.checkIntegrationApprovedRead(ctx, q, r)
	}
	if a.lease != nil {
		return s.integrationExecutionTx(ctx, q, r)
	}
	if a.store != s || a.lease != nil || !r.commandOnly || r.WorldScope == nil || *r.WorldScope != a.scope || r.ProductScope != a.scope.ProductScope || r.OwnerSubjectID != a.scope.SubjectID || ctx.Value(integrationTurnKey{}) != a || ctx.Value(operatorContextKey{}) != nil || !(a.action == "QUOTE" && r.Operation == "TURN_QUOTE" || a.action == "TURN" && r.Operation == "HARNESS_INTAKE") {
		return ErrDenied
	}
	n, e := s.inspectIntegrationTurn(ctx, q, a.principal, a.scope.WorldInstanceID, false, a.choice)
	if e != nil || !reflect.DeepEqual(n.session, a.session) || !reflect.DeepEqual(n.eligibility, a.eligibility) || !reflect.DeepEqual(n.service, a.service) || n.account != a.account || n.use != a.use {
		return ErrDenied
	}
	if a.fresh != nil {
		if e := a.fresh.check(ctx, q, r); e != nil {
			return e
		}
	}
	if a.action == "QUOTE" {
		return a.verifyQuote(ctx, q, r, false)
	}
	if e := a.verifyQuote(ctx, q, r, true); e != nil {
		return e
	}
	return a.verifyIntake(ctx, q, r, turnIdentity(a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.WorldInstanceID, "TURN", a.key), false)
}

// EnterIntegrationTaskIssuer derives a worker seal only from the original
// registered task and its live lease. HTTP principals cannot mint this seal.
func (s *Store) EnterIntegrationTaskIssuer(ctx context.Context, lease domain.TaskLease, worker string) (context.Context, error) {
	if s.integrationClosed.Load() {
		return ctx, ErrDenied
	}
	if a, ok := ctx.Value(integrationTurnKey{}).(*integrationTurnPlan); ok {
		if a.store != s || a.lease == nil || !reflect.DeepEqual(*a.lease, lease) || a.worker != worker {
			return ctx, ErrDenied
		}
		return ctx, nil
	}
	var provenance sql.NullString
	var operation string
	e := s.DB.QueryRowContext(ctx, "SELECT issuer_provenance,operation FROM registered_jobs WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&provenance, &operation)
	if e != nil {
		return ctx, e
	}
	// Legacy tasks may have NULL provenance; their original owning gate remains in charge.
	if !provenance.Valid || provenance.String != "DIRECT_INTEGRATION" {
		return ctx, nil
	}
	if ctx.Value(integrationContextKey{}) != nil || ctx.Value(operatorContextKey{}) != nil || s.integrationClosed.Load() {
		return ctx, ErrDenied
	}
	if operation != "TURN" || lease.WorldInstanceID == nil || lease.SubjectID != lease.OwnerSubjectID || lease.Fence == 0 || worker == "" {
		return ctx, ErrDenied
	}
	var id string
	e = s.DB.QueryRowContext(ctx, "SELECT principal_ref FROM integration_task_principals WHERE tenant_id=? AND product_id=? AND task_id=? AND task_kind='TURN' AND principal_kind='CLIENT'", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&id)
	if e != nil {
		return ctx, ErrDenied
	}
	v := currentIntegration{store: s}
	p := &v.principal
	var tokenHash []byte
	e = s.DB.QueryRowContext(ctx, "SELECT principal_id,kind,tenant_id,product_id,customer_id,subject_id,COALESCE(world_instance_id,''),profile,expires_at,token_hash FROM integration_principals WHERE principal_id=?", id).Scan(&p.PrincipalID, &p.Kind, &p.Tenant, &p.Product, &p.Customer, &p.StableSubject, &p.World, &p.Profile, &p.ExpiresAt, &tokenHash)
	if e != nil || len(tokenHash) != 32 || p.Kind != domain.IntegrationClient || p.Tenant != lease.TenantID || p.Product != lease.ProductID || p.StableSubject != lease.OwnerSubjectID || p.World != *lease.WorldInstanceID {
		return ctx, ErrDenied
	}
	copy(v.hash[:], tokenHash)
	proofCtx := context.WithValue(ctx, integrationContextKey{}, v)
	a, e := s.inspectIntegrationTurn(proofCtx, s.DB, v, *lease.WorldInstanceID, true)
	if e != nil {
		return ctx, e
	}
	var rawHash []byte
	e = s.DB.QueryRowContext(ctx, "SELECT original_key,quote_id,command_hash FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND world_instance_id=?", lease.TenantID, lease.ProductID, lease.TaskID, lease.OwnerSubjectID, *lease.WorldInstanceID).Scan(&a.key, &a.quote, &rawHash)
	if e != nil || len(rawHash) != 32 {
		return ctx, ErrDenied
	}
	copy(a.hash[:], rawHash)
	a.task = lease.TaskID
	a.action = "TURN"
	a.worker = worker
	l := lease
	a.lease = &l
	if a.task != turnIdentity(lease.TenantID, lease.ProductID, lease.SubjectID, *lease.WorldInstanceID, "TURN", a.key) {
		return ctx, ErrDenied
	}
	a.keys = append(a.keys, harnessTaskKey(a.scope.ProductScope, a.quote), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(a.task, "accepted")}})
	var quoteReceipt string
	if e = s.DB.QueryRowContext(ctx, "SELECT quote_receipt FROM integration_quotes WHERE tenant_id=? AND product_id=? AND quote_id=?", lease.TenantID, lease.ProductID, a.quote).Scan(&quoteReceipt); e != nil {
		return ctx, e
	}
	a.keys = append(a.keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{quoteReceipt}})
	// Seal issuance owns the same current rows as subsequent execution. The
	// preliminary DB reads collect addresses only; they cannot issue authority.
	a.keys = append(a.keys, LockKey{Rank: 40, IDs: []string{a.scope.TenantID, a.scope.ProductID, a.scope.WorldInstanceID}}, harnessTaskKey(a.scope.ProductScope, a.task))
	tx, e := s.integrationMemoryReadTx(ctx, a.keys)
	if e != nil {
		return ctx, e
	}
	defer tx.Rollback()
	check := func() error {
		n, err := s.inspectIntegrationTurn(proofCtx, tx, v, a.scope.WorldInstanceID, true, a.choice)
		if err != nil || !reflect.DeepEqual(n.session, a.session) || !reflect.DeepEqual(n.eligibility, a.eligibility) || !reflect.DeepEqual(n.service, a.service) || n.use != a.use || n.account != a.account || s.integrationClosed.Load() {
			return ErrDenied
		}
		if err = a.verifyIntake(proofCtx, tx, a.intakeRequest(), a.task, true); err != nil {
			return err
		}
		return a.currentLease(ctx, tx)
	}
	if e = check(); e != nil {
		return ctx, e
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return ctx, e
		}
	}
	if e = check(); e != nil {
		return ctx, e
	}
	return context.WithValue(ctx, integrationTurnKey{}, a), nil
}

func (a *integrationTurnPlan) intakeRequest() BarrierRequest {
	return BarrierRequest{ProductScope: a.scope.ProductScope, WorldScope: &a.scope, OwnerSubjectID: a.scope.SubjectID, Operation: "HARNESS_INTAKE", CommandKey: a.key, CommitKey: turnIdentity(a.task, "accepted"), PayloadHash: a.hash, commandOnly: true}
}

func (a *integrationTurnPlan) currentLease(ctx context.Context, q creationRows, committed ...bool) error {
	var live bool
	advanced := len(committed) == 1 && committed[0]
	e := q.QueryRowContext(ctx, `SELECT (t.state='RUNNING' OR (? AND t.state='COMMITTED')) AND t.fence=? AND t.lease_owner=? AND t.lease_expires_at>UTC_TIMESTAMP(6) AND t.cancelled=0 AND i.original_execute_by>UTC_TIMESTAMP(6) AND i.original_settle_by>UTC_TIMESTAMP(6) FROM tasks t JOIN integration_task_principals i USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?`, advanced, a.lease.Fence, a.worker, a.scope.TenantID, a.scope.ProductID, a.task).Scan(&live)
	if e != nil || !live {
		return ErrDenied
	}
	return nil
}

// The command is an internal address for the sealed worker; its exported fields
// alone cannot select the integration branch in any subsequent transaction.
func (s *Store) IntegrationWorkerCommand(ctx context.Context, scope domain.Scope, key string) (CommandBarrierRequest, bool, error) {
	a, ok := ctx.Value(integrationTurnKey{}).(*integrationTurnPlan)
	if !ok {
		return CommandBarrierRequest{}, false, nil
	}
	if a.store != s || a.lease == nil || a.scope != scope {
		return CommandBarrierRequest{}, true, ErrDenied
	}
	c, e := s.integrationTurnCommand(ctx, a, key)
	return c, true, e
}

func integrationExecutionOperation(operation string) bool {
	switch operation {
	case "HARNESS_CONTEXT", "TURN_DISPATCH", "HARNESS_SEND_CONSUME", "HARNESS_RESULT_STAGE", "HARNESS_RESULT_VALIDATE", "HARNESS_RESULT_READ", "HARNESS_CANDIDATE_APPROVE", "HARNESS_TURN_COMMIT", "HARNESS_RECOVERY_INSPECT", "HARNESS_RECOVERY_APPROVE", "HARNESS_LOCAL_PREPARE", "HARNESS_LOCAL_ROLLBACK":
		return true
	}
	return false
}

func (s *Store) prepareIntegrationExecution(ctx context.Context, r *BarrierRequest) error {
	a := r.integrationTurn
	if a == nil {
		a, _ = ctx.Value(integrationTurnKey{}).(*integrationTurnPlan)
	}
	if a == nil || a.lease == nil {
		return nil
	}
	if a.store != s || r.commandOnly || r.WorldScope == nil || *r.WorldScope != a.scope || r.ProductScope != a.scope.ProductScope || r.OwnerSubjectID != a.scope.SubjectID || r.TaskID != a.task || r.WorkerID != a.worker || r.Fence != a.lease.Fence || !integrationExecutionOperation(r.Operation) {
		return ErrDenied
	}
	r.integrationTurn = a
	r.Locks = append(r.Locks, a.keys...)
	return nil
}

// Both the initial and final owning checks use the accepted Session and use
// generation. Natural Session expiry alone does not revoke an accepted task.
func (s *Store) integrationExecutionTx(ctx context.Context, q creationRows, r BarrierRequest) error {
	a := r.integrationTurn
	if a == nil || a.lease == nil || a.store != s || s.integrationClosed.Load() || ctx.Value(operatorContextKey{}) != nil || ctx.Value(integrationContextKey{}) != nil {
		return ErrDenied
	}
	if e := s.prepareIntegrationExecution(ctx, &r); e != nil {
		return e
	}
	proofCtx := context.WithValue(ctx, integrationContextKey{}, a.principal)
	n, e := s.inspectIntegrationTurn(proofCtx, q, a.principal, a.scope.WorldInstanceID, true, a.choice)
	if e != nil || !reflect.DeepEqual(n.session, a.session) || !reflect.DeepEqual(n.eligibility, a.eligibility) || !reflect.DeepEqual(n.service, a.service) || n.account != a.account || n.use.Generation != a.use.Generation {
		return ErrDenied
	}
	advanced := r.Operation == "HARNESS_TURN_COMMIT" && r.localCandidateRound != nil && r.localCandidateRound.phase == "WORLD_COMMIT" && r.localCandidateRound.used.Load()
	if advanced {
		proof, e := queryReceipt(ctx, q, r.CommitKey)
		if e != nil || !proof.matches(r) {
			return ErrDenied
		}
		var bound bool
		e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND receipt_key=?)", a.scope.TenantID, a.scope.ProductID, a.task, r.CommitKey).Scan(&bound)
		if e != nil || !bound {
			return ErrDenied
		}
	}
	if e = a.currentLease(ctx, q, advanced); e != nil {
		return e
	}
	if e = a.verifyQuote(proofCtx, q, r, true); e != nil {
		return e
	}
	copy := *a
	copy.use = n.use
	return copy.verifyIntake(proofCtx, q, a.intakeRequest(), a.task, true)
}

func (s *Store) acquireIntegrationTurnKey(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) (bool, error) {
	a := r.integrationTurn
	if a == nil {
		return false, nil
	}
	if a.lease != nil && k.Rank == 50 && reflect.DeepEqual(k, harnessTaskKey(a.scope.ProductScope, a.quote)) {
		if a.store != s {
			return true, ErrDenied
		}
		if e := marker(ctx, tx, k); e != nil {
			return true, e
		}
		return true, studioLockRows(ctx, tx, "SELECT quote_id FROM harness_quotes WHERE tenant_id=? AND product_id=? AND quote_id=? AND owner_subject_id=? FOR UPDATE", a.scope.TenantID, a.scope.ProductID, a.quote, a.scope.SubjectID)
	}
	switch k.Kind {
	case "integration_principal", "integration_entitlement", "integration_customer", "integration_mapping", "integration_participant", "integration_consent", "integration_receipt", "content_check":
	default:
		return false, nil
	}
	found := false
	for _, x := range a.keys {
		if reflect.DeepEqual(x, k) {
			found = true
		}
	}
	if !found || a.store != s {
		return true, ErrDenied
	}
	if e := marker(ctx, tx, k); e != nil {
		return true, e
	}
	if k.Kind == "integration_participant" {
		return true, studioLockRows(ctx, tx, "SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
	}
	if k.Kind == "content_check" {
		return true, studioLockRows(ctx, tx, "SELECT result_id FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
	}
	if k.Kind == "integration_receipt" {
		return true, studioLockRows(ctx, tx, "SELECT receipt_ref FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND original_key=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3])
	}
	if k.Kind == "integration_consent" {
		return true, studioLockRows(ctx, tx, "SELECT player_subject_id FROM integration_self_consent_current WHERE tenant_id=? AND product_id=? AND player_subject_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
	}
	return true, lockStudioRows(ctx, tx, k)
}

func (a *integrationTurnPlan) writeQuote(ctx context.Context, u *unit, r BarrierRequest, request generated.QuoteRequest, out generated.Quote, project string, expiry time.Time) error {
	if u.integrationTurn != a || a.action != "QUOTE" || u.operation != "TURN_QUOTE" {
		return ErrDenied
	}
	bounds, _ := json.Marshal(request)
	_, e := u.tx.ExecContext(ctx, `INSERT INTO integration_quotes VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, a.scope.TenantID, a.scope.ProductID, out.QuoteID, a.service.principal.PrincipalID, a.service.principal.Customer, a.scope.SubjectID, a.scope.WorldInstanceID, project, a.eligibility.entitlement.Release, out.RouteID, r.PayloadHash[:], string(bounds), a.account, expiry, r.CommitKey)
	return e
}

func (a *integrationTurnPlan) verifyQuote(ctx context.Context, q creationRows, r BarrierRequest, required bool) error {
	id := a.quote
	if !required {
		id = turnIdentity(a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.WorldInstanceID, "TURN_QUOTE", a.key)
	}
	var service, customer, player, world, project, release, route, account, receipt string
	var hash, bounds, raw []byte
	var expiry time.Time
	e := q.QueryRowContext(ctx, `SELECT i.issuer_service_key,i.customer_id,i.player_subject_id,i.world_instance_id,i.project_id,i.release_id,i.route,i.request_hash,i.bounds,i.customer_account_id,i.expires_at,i.quote_receipt,t.public_document FROM integration_quotes i JOIN harness_turn_quotes t USING(tenant_id,product_id,quote_id) WHERE i.tenant_id=? AND i.product_id=? AND i.quote_id=?`, a.scope.TenantID, a.scope.ProductID, id).Scan(&service, &customer, &player, &world, &project, &release, &route, &hash, &bounds, &account, &expiry, &receipt, &raw)
	if errors.Is(e, sql.ErrNoRows) && !required {
		return nil
	}
	var req generated.QuoteRequest
	var out generated.Quote
	if e != nil || service != a.service.principal.PrincipalID || customer != a.service.principal.Customer || player != a.scope.SubjectID || world != a.scope.WorldInstanceID || release != a.eligibility.entitlement.Release || account != a.account || json.Unmarshal(bounds, &req) != nil || generated.Decode("Quote", raw, &out) != nil || out.QuoteID != id || out.RouteID != route {
		return ErrDenied
	}
	_, expected, e := harnessPayload(struct {
		Scope   domain.Scope
		Request generated.QuoteRequest
	}{a.scope, req})
	if e != nil || !bytes.Equal(hash, expected[:]) {
		return ErrDenied
	}
	var qraw []byte
	var good bool
	e = q.QueryRowContext(ctx, "SELECT document,(owner_subject_id=? AND world_instance_id=? AND project_id=? AND expires_at=?) FROM harness_quotes WHERE tenant_id=? AND product_id=? AND quote_id=?", player, world, project, expiry, a.scope.TenantID, a.scope.ProductID, id).Scan(&qraw, &good)
	var original HarnessQuote
	if e != nil || !good || json.Unmarshal(qraw, &original) != nil || original.ID != id || original.ProjectID != project || original.RequestHash != expected || hex.EncodeToString(original.RouteHash[:]) != route || original.ServiceAtoms != 0 || original.SupplierAtoms != 0 {
		return ErrDenied
	}
	// Public quotation fields remain bound to their original bounded producer.
	expectedOutput := generated.Quote{QuoteID: id, Scope: req.Scope, RouteID: route, PurchaseResponsibility: "MOCK", RateBookVersion: "r0-mock-scale8-v1", ContractReference: "r0-bounded-turn-v1", ServiceUnit: "WE_CREDIT", Lines: []generated.QuoteLine{{SKU: "TURN_TEXT", Meter: "INPUT_TOKEN", IncludedUnits: "0", MaxMeterUnits: req.InputUpperBound, MaxServiceUnits: "0", RateUnits: "0", CompletionCondition: "APPROVED_RESULT_RETRIEVABLE"}, {SKU: "TURN_TEXT", Meter: "OUTPUT_TOKEN", IncludedUnits: "0", MaxMeterUnits: req.OutputUpperBound, MaxServiceUnits: "0", RateUnits: "0", CompletionCondition: "APPROVED_RESULT_RETRIEVABLE"}}, EstimatedServiceUnits: "0", MaxServiceUnits: "0", SupplierCostCapUnits: "0", SupplierCostCurrency: "USD", Rounding: "SUM_THEN_HALF_EVEN", ExpiresAt: expiry.Format(time.RFC3339Nano), ReservationSettleBy: expiry.Add(180 * time.Second).Format(time.RFC3339Nano), RiskBearer: "PLATFORM", CommercialStatus: "SYNTHETIC_MOCK"}
	if !reflect.DeepEqual(out, expectedOutput) || receipt != turnIdentity(id, "receipt") {
		return ErrDenied
	}
	if required {
		proof, e := queryReceipt(ctx, q, receipt)
		if e != nil || domain.ValidateIdempotencyKey(proof.command) != nil || id != turnIdentity(a.scope.TenantID, a.scope.ProductID, player, world, "TURN_QUOTE", proof.command) || !proof.matches(BarrierRequest{ProductScope: a.scope.ProductScope, WorldScope: &a.scope, OwnerSubjectID: player, Operation: "TURN_QUOTE", CommandKey: proof.command, CommitKey: receipt, PayloadHash: expected, commandOnly: true}) {
			return ErrDenied
		}
	}
	a.output = out
	return nil
}

func (a *integrationTurnPlan) accounts(project string) []Reservation {
	out := r0Accounts(a.scope, project)
	for i := range out {
		if out[i].Kind == "customer" {
			out[i].AccountID = a.account
		}
	}
	return out
}
func (a *integrationTurnPlan) parentHolds(project, task string) []Reservation {
	var out []Reservation
	for _, h := range a.accounts(project) {
		if h.Kind == "supplier_global" || h.Kind == "supplier_tenant" {
			continue
		}
		h.ReservationID = turnIdentity(task, "parent", h.Kind)
		out = append(out, h)
	}
	return out
}

func (a *integrationTurnPlan) writeIntake(ctx context.Context, u *unit, r BarrierRequest, task string, now time.Time) error {
	if a.action != "TURN" || a.fresh != nil || u.operatorStore != a.store || u.operation != "HARNESS_INTAKE" || !u.commandOnly || u.scope != a.scope.ProductScope || u.world != a.scope.WorldInstanceID || r.integrationTurn != a || r.Operation != u.operation || r.CommandKey != a.key || r.CommitKey != turnIdentity(task, "accepted") || task != turnIdentity(a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.WorldInstanceID, "TURN", a.key) || u.integrationTurn != a || a.principal.principal.Kind != domain.IntegrationClient {
		return ErrDenied
	}
	reg, _ := json.Marshal(integrationRegistrationProof{a.session.CustomerRevision, a.session.MappingRevision, a.session.EntitlementRevision, a.session.Entitlement, a.session.Installation, a.scope.WorldInstanceID})
	_, e := u.tx.ExecContext(ctx, `INSERT INTO integration_task_principals VALUES(?,?,?,'TURN','CLIENT',?,?,?,?,?,'TURN',?,?,?,?,?,?,CAST(? AS JSON),?,?,?)`, a.scope.TenantID, a.scope.ProductID, task, a.session.ID, a.service.principal.PrincipalID, a.service.principal.Customer, a.scope.SubjectID, a.scope.WorldInstanceID, now, a.session.Expires, now.Add(120*time.Second), now.Add(180*time.Second), r.CommitKey, r.PayloadHash[:], string(reg), a.use.Generation, a.use.Expires, a.use.Revision)

	if e != nil {
		return e
	}
	a.fresh = &integrationFreshWrite{store: a.store, unit: u, tx: u.tx, turn: a, scope: a.scope, task: task, unitTask: u.taskID, operation: u.operation, command: r.CommandKey, receipt: r.CommitKey, kind: "TURN", principal: "CLIENT", ref: a.session.ID, service: a.service.principal.PrincipalID, customer: a.service.principal.Customer, player: a.scope.SubjectID, action: "TURN", hash: r.PayloadHash, registration: integrationRegistrationProof{a.session.CustomerRevision, a.session.MappingRevision, a.session.EntitlementRevision, a.session.Entitlement, a.session.Installation, a.scope.WorldInstanceID}, accepted: now, issued: a.session.Expires, execute: now.Add(120 * time.Second), settle: now.Add(180 * time.Second), use: a.use}
	return nil
}

func (s *Store) integrationOriginalTurn(ctx context.Context, c CommandBarrierRequest, key string, hash [32]byte) (generated.TurnAccepted, error) {
	a := c.integrationTurn
	if a == nil {
		return s.OriginalTurn(ctx, *c.WorldScope, key, hash)
	}
	id := turnIdentity(a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, a.scope.WorldInstanceID, "TURN", key)
	keys := append([]LockKey{}, a.keys...)
	keys = append(keys, LockKey{Rank: 40, IDs: []string{a.scope.TenantID, a.scope.ProductID, a.scope.WorldInstanceID}}, harnessTaskKey(a.scope.ProductScope, id), harnessTaskKey(a.scope.ProductScope, a.quote), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(id, "accepted")}})
	tx, e := s.integrationMemoryReadTx(ctx, keys)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	defer tx.Rollback()
	n, e := s.inspectIntegrationTurn(ctx, tx, a.principal, a.scope.WorldInstanceID, false, a.choice)
	if e != nil || n.account != a.account {
		return generated.TurnAccepted{}, ErrDenied
	}
	var stored []byte
	var quote string
	e = tx.QueryRowContext(ctx, "SELECT command_hash,quote_id FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND world_instance_id=? AND original_key=?", a.scope.TenantID, a.scope.ProductID, id, a.scope.SubjectID, a.scope.WorldInstanceID, key).Scan(&stored, &quote)
	if e != nil {
		return generated.TurnAccepted{}, e
	}
	if !bytes.Equal(stored, hash[:]) || quote != a.quote {
		return generated.TurnAccepted{}, ErrConflict
	}
	r := harnessCommand(c)
	r.Operation = "HARNESS_INTAKE"
	r.PayloadHash = hash
	r.CommitKey = turnIdentity(id, "accepted")
	if e = a.verifyIntake(ctx, tx, r, id, true); e != nil {
		return generated.TurnAccepted{}, e
	}
	if s.BeforeReadFinish != nil {
		if e = s.BeforeReadFinish(ctx, tx); e != nil {
			return generated.TurnAccepted{}, e
		}
	}
	if _, e = s.inspectIntegrationTurn(ctx, tx, a.principal, a.scope.WorldInstanceID, false, a.choice); e != nil {
		return generated.TurnAccepted{}, e
	}
	if e = a.verifyIntake(ctx, tx, r, id, true); e != nil {
		return generated.TurnAccepted{}, e
	}
	return generated.TurnAccepted{TurnID: id, QuoteID: quote, Task: generated.TaskReceipt{TaskID: id, TaskKind: "TURN", TaskState: "ACCEPTED", Scope: turnScope(a.scope), CorrelationID: id}}, nil
}

func (a *integrationTurnPlan) verifyIntake(ctx context.Context, q creationRows, r BarrierRequest, id string, required bool, metadataOnly ...bool) error {
	metadata := len(metadataOnly) == 1 && metadataOnly[0]
	var good bool
	var generation, revision uint64
	var expiry, accepted, issued time.Time
	var ref string
	var registration []byte
	e := q.QueryRowContext(ctx, `SELECT (i.task_kind='TURN' AND i.principal_kind='CLIENT' AND i.issuer_service_key=? AND i.customer_id=? AND i.player_subject_id=? AND i.world_instance_id=? AND i.action='TURN' AND i.intake_receipt=? AND i.intake_hash=? AND i.original_execute_by=e.execute_by AND i.original_settle_by=e.settle_by AND i.accepted_at=e.accepted_at AND e.execute_by=DATE_ADD(e.accepted_at,INTERVAL 120 SECOND) AND e.settle_by=DATE_ADD(e.accepted_at,INTERVAL 180 SECOND) AND t.owner_subject_id=? AND t.task_kind='TURN' AND t.world_instance_id=i.world_instance_id AND j.issuer_provenance='DIRECT_INTEGRATION' AND j.issuer_credential_id IS NULL AND j.operation='TURN' AND h.command_hash=i.intake_hash AND h.accepted_commit_key=i.intake_receipt AND h.original_key=? AND h.quote_id=?),i.accepted_use_generation,i.accepted_consent_revision,i.accepted_use_expires_at,i.accepted_at,i.issued_session_expires_at,i.principal_ref,i.registration_revisions FROM integration_task_principals i JOIN tasks t USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN harness_turns h USING(tenant_id,product_id,task_id) JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE i.tenant_id=? AND i.product_id=? AND i.task_id=?`, a.service.principal.PrincipalID, a.service.principal.Customer, a.scope.SubjectID, a.scope.WorldInstanceID, r.CommitKey, r.PayloadHash[:], a.scope.SubjectID, a.key, a.quote, a.scope.TenantID, a.scope.ProductID, id).Scan(&good, &generation, &revision, &expiry, &accepted, &issued, &ref, &registration)
	if errors.Is(e, sql.ErrNoRows) && !required {
		return nil
	}
	if e != nil || !good || generation != a.use.Generation || revision > a.use.Revision || !expiry.After(accepted) {
		return ErrDenied
	}
	var originalKey string
	e = q.QueryRowContext(ctx, "SELECT x.original_key FROM integration_principals p JOIN integration_receipts x ON x.receipt_ref=p.issuance_receipt AND x.tenant_id=p.tenant_id AND x.product_id=p.product_id AND x.owner_id=p.issuer_service_key AND x.operation='EXCHANGE_SESSION' WHERE p.principal_id=? AND p.kind='CLIENT'", ref).Scan(&originalKey)
	if e != nil {
		return ErrDenied
	}
	original, e := sessionRead(ctx, q, a.service.principal, originalKey)
	if e != nil || original.ID != ref || original.Player != a.scope.SubjectID || original.World != a.scope.WorldInstanceID || original.Entitlement != a.eligibility.entitlement.ID || !original.Expires.Equal(issued) || accepted.Before(original.Issued) || !accepted.Before(original.Expires) {
		return ErrDenied
	}
	// The accepted sidecar keeps the original issuance tuple, even when a
	// later lawful Session or active RENEW is used for a current read.
	var registered integrationRegistrationProof
	want := integrationRegistrationProof{original.CustomerRevision, original.MappingRevision, original.EntitlementRevision, original.Entitlement, original.Installation, a.scope.WorldInstanceID}
	if json.Unmarshal(registration, &registered) != nil || registered != want {
		return ErrDenied
	}
	var factOK bool
	e = q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM integration_self_consent_facts f JOIN integration_receipts x ON x.tenant_id=f.tenant_id AND x.product_id=f.product_id AND x.owner_id=f.player_subject_id AND x.original_key=f.original_key AND x.payload_hash=f.payload_hash AND x.receipt_ref=f.receipt_ref AND x.target_revision=f.revision AND x.operation='PLAYER_SELF_CONSENT' AND x.state='COMMITTED' WHERE f.tenant_id=? AND f.product_id=? AND f.player_subject_id=? AND f.revision=? AND f.use_authority_generation=? AND f.expires_at=? AND f.action IN ('INITIAL','RENEW') AND f.policy=?) AND (? OR ?>UTC_TIMESTAMP(6))`, a.scope.TenantID, a.scope.ProductID, a.scope.SubjectID, revision, generation, expiry, playerConsentPolicy, metadata, expiry).Scan(&factOK)
	if e != nil || !factOK {
		return ErrDenied
	}
	if e = a.verifyAcceptedConsent(ctx, q, revision, generation, expiry); e != nil {
		return e
	}
	proof, e := queryReceipt(ctx, q, r.CommitKey)
	if errors.Is(e, sql.ErrNoRows) && !required {
		return nil
	}
	if e != nil || !proof.matches(r) {
		return ErrDenied
	}
	return nil
}

// Provision only the exact installed zero-cost mock accounts selected by the
// real quote. The customer account must already exist from REGISTER_CUSTOMER.
func provisionIntegrationTurnAccount(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	a := r.integrationTurn
	if a == nil || a.store == nil || a.action != "TURN" || !r.commandOnly || r.Operation != "HARNESS_INTAKE" {
		return ErrDenied
	}
	var project string
	if e := tx.QueryRowContext(ctx, "SELECT project_id FROM integration_quotes WHERE tenant_id=? AND product_id=? AND quote_id=? AND customer_account_id=?", a.scope.TenantID, a.scope.ProductID, r.harnessIntakeQuote, a.account).Scan(&project); e != nil {
		return e
	}
	found := false
	for _, h := range a.accounts(project) {
		if h.Kind == k.Kind && h.ScopeKey == k.IDs[0] && h.AccountID == k.IDs[1] {
			found = true
		}
	}
	if !found || k.Kind == "customer" {
		return ErrDenied
	}
	var tenant, product, world, proj any
	if k.Kind != "supplier_global" && k.Kind != "platform_risk" {
		tenant = a.scope.TenantID
		if k.Kind != "supplier_tenant" {
			product = a.scope.ProductID
		}
	}
	if k.Kind == "world" {
		world = a.scope.WorldInstanceID
	}
	if k.Kind == "project" {
		proj = project
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,world_instance_id,project_id,balance,held) VALUES(?,?,?,?,?,?,?,0,0)", k.Kind, k.IDs[0], k.IDs[1], tenant, product, world, proj)
	return e
}
