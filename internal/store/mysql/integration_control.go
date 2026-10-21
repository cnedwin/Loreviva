package mysql

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"reflect"
	"strconv"
	"time"
)

// This seal is issued only by the Store after resolving a real bearer or an
// original registered task. Neither exported request fields nor a Scope mint it.
type integrationControlKey struct{}
type integrationControlPlan struct {
	fresh       *integrationFreshWrite
	store       *Store
	principal   currentIntegration
	service     currentIntegration
	session     sessionRecord
	eligibility sessionEligibility
	document    integrationControlDocument
	raw         []byte
	hash        [32]byte
	keys        []LockKey
	lease       *RegisteredLease
	worker      string
}
type integrationControlDocument struct {
	Scope                  domain.Scope
	Versions               domain.Versions
	Status, Action, PlanID string
	Key, Task, Receipt     string
	Request                generated.ControlRequest
}
type integrationRegistrationProof struct {
	Customer, Mapping, Entitlement               uint64
	EntitlementID, Installation, CreationReceipt string
}

func (a *integrationControlPlan) registration() integrationRegistrationProof {
	return integrationRegistrationProof{a.eligibility.mapping.CustomerRevision, a.eligibility.mapping.MappingRevision, a.eligibility.entitlement.Revision, a.eligibility.entitlement.ID, a.eligibility.identity.Installation, a.document.Scope.WorldInstanceID}
}
func integrationControlHash(d integrationControlDocument) ([32]byte, error) {
	b, e := json.Marshal(struct {
		Scope     domain.Scope
		Operation string
		Input     generated.ControlRequest
	}{d.Scope, "controlIntegrationInstance", d.Request})
	if e != nil {
		return [32]byte{}, e
	}
	b, e = wire.Canonical(b)
	return sha256.Sum256(b), e
}
func integrationControlAction(kind domain.IntegrationKind, action string) bool {
	return kind == domain.IntegrationService && (action == "START" || action == "END") || kind == domain.IntegrationClient && (action == "PAUSE" || action == "RESUME")
}

// Player registration intentionally never grants general can_control. The
// Store-issued plan authorizes only this player's exact private PAUSE/RESUME;
// its registration/action proof is rechecked under locks before every effect.
func (a *integrationControlPlan) clientControl(s domain.ProductScope, world string) bool {
	return a != nil && a.store != nil && a.principal.principal.Kind == domain.IntegrationClient && a.document.Scope.ProductScope == s && a.document.Scope.WorldInstanceID == world && a.session.Player == s.SubjectID && a.session.World == world && (a.document.Action == "PAUSE" || a.document.Action == "RESUME")
}

func (s *Store) inspectIntegrationControl(ctx context.Context, q creationRows, v currentIntegration, world, action string, background bool, metadata ...bool) (*integrationControlPlan, error) {
	a := &integrationControlPlan{store: s, principal: v}
	readOnly := len(metadata) == 1 && metadata[0]
	if !integrationControlAction(v.principal.Kind, action) || world == "" {
		return nil, ErrDenied
	}
	var e error
	if v.principal.Kind == domain.IntegrationClient {
		a.session, a.eligibility, e = s.currentClientForTask(ctx, q, v, background)
		if e != nil || a.session.World != world {
			return nil, ErrDenied
		}
		allowed := false
		for _, x := range a.session.Actions {
			if (!readOnly && x == "CONTROL_"+action) || (readOnly && x == "READ") {
				allowed = true
			}
		}
		if !allowed {
			return nil, ErrDenied
		}
		a.service, e = s.integrationServiceProof(ctx, q, a.session.Service)
	} else {
		a.service = v
		var external, entitlement string
		e = q.QueryRowContext(ctx, "SELECT external_id,entitlement_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND customer_id=? AND service_owner_id=?", v.principal.Tenant, v.principal.Product, world, v.principal.Customer, v.principal.StableSubject).Scan(&external, &entitlement)
		if e == nil {
			a.eligibility, e = s.sessionEligibility(ctx, q, v, generated.ClientSessionRequest{ProductID: v.principal.Product, WorldInstanceID: world, ExternalSubjectID: external, EntitlementReference: entitlement})
		}
	}
	if e != nil {
		return nil, e
	}
	var actions []string
	if json.Unmarshal([]byte(a.eligibility.entitlement.Actions), &actions) != nil {
		return nil, ErrDenied
	}
	if v.principal.Kind == domain.IntegrationClient {
		found := false
		for _, x := range actions {
			if (!readOnly && x == "CONTROL_"+action) || (readOnly && x == "READ") {
				found = true
			}
		}
		if !found {
			return nil, ErrDenied
		}
	}
	p := a.service.principal
	m := a.eligibility.mapping
	a.keys = []LockKey{{Rank: 15, Kind: "integration_principal", IDs: []string{p.PrincipalID}}, {Rank: 25, Kind: "integration_entitlement", IDs: []string{p.Tenant, p.Product, p.Customer, a.eligibility.entitlement.ID}}, {Rank: 30, IDs: []string{p.Tenant, p.Product, p.StableSubject}}, {Rank: 30, IDs: []string{p.Tenant, p.Product, a.eligibility.entitlement.Player}}, {Rank: 35, Kind: "integration_customer", IDs: []string{p.Tenant, p.Product, p.Customer}}, {Rank: 35, Kind: "integration_mapping", IDs: []string{p.Tenant, p.Product, p.Customer, creationExternalHash([]byte(m.External))}}, {Rank: 35, Kind: "integration_participant", IDs: []string{p.Tenant, p.Product, world}}}
	if v.principal.Kind == domain.IntegrationClient {
		a.keys = append(a.keys, LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{a.session.ID}}, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{p.Tenant, p.Product, p.PrincipalID, a.session.Key}})
	}
	a.keys = append(a.keys, a.eligibility.keys...)
	return a, nil
}
func (s *Store) integrationServiceProof(ctx context.Context, q rowQuerier, id string) (currentIntegration, error) {
	v := currentIntegration{store: s}
	var hash []byte
	p := &v.principal
	e := q.QueryRowContext(ctx, "SELECT principal_id,kind,tenant_id,product_id,customer_id,subject_id,profile,expires_at,token_hash FROM integration_principals WHERE principal_id=? AND kind='SERVICE'", id).Scan(&p.PrincipalID, &p.Kind, &p.Tenant, &p.Product, &p.Customer, &p.StableSubject, &p.Profile, &p.ExpiresAt, &hash)
	if e != nil || len(hash) != 32 {
		return v, ErrDenied
	}
	copy(v.hash[:], hash)
	return v, nil
}
func (s *Store) ControlIntegrationInstance(ctx context.Context, world, key string, input generated.ControlRequest) (generated.TaskReceipt, error) {
	var out generated.TaskReceipt
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return out, e
	}
	defer done()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	b, e := json.Marshal(input)
	if !ok || v.store != s || domain.ValidateIdempotencyKey(key) != nil || e != nil || generated.Validate("ControlRequest", b) != nil {
		return out, ErrDenied
	}
	a, e := s.inspectIntegrationControl(ctx, s.DB, v, world, input.Command, false)
	if e != nil {
		return out, e
	}
	d := integrationControlDocument{Scope: domain.Scope{ProductScope: domain.ProductScope{TenantID: v.principal.Tenant, ProductID: v.principal.Product, SubjectID: v.principal.StableSubject}, WorldInstanceID: world}, Key: key, Action: input.Command, Request: input}
	d.Task = creationID(d.Scope.TenantID, d.Scope.ProductID, d.Scope.SubjectID, world, "controlIntegrationInstance", key)
	d.Receipt = creationID(d.Task, "accept")
	a.hash, e = integrationControlHash(d)
	if e != nil {
		return out, e
	}
	var old []byte
	e = s.DB.QueryRowContext(ctx, "SELECT document FROM runtime_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", d.Scope.TenantID, d.Scope.ProductID, d.Task).Scan(&old)
	if e == nil {
		var original integrationControlDocument
		if json.Unmarshal(old, &original) != nil {
			return out, ErrDenied
		}
		h, err := integrationControlHash(original)
		if err != nil || h != a.hash {
			return out, ErrConflict
		}
		d = original
	} else if errors.Is(e, sql.ErrNoRows) {
		e = s.DB.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", d.Scope.TenantID, d.Scope.ProductID, world).Scan(&d.Versions.ReleaseID, &d.Versions.WorldRevision, &d.Versions.ControlVersion, &d.Versions.PrivacyEpoch, &d.Versions.ViewEpoch, &d.Status)
		want, err := strconv.ParseUint(input.ExpectedControlVersion, 10, 64)
		if e != nil || err != nil || want != d.Versions.ControlVersion {
			return out, ErrConflict
		}
		if !(input.Command == "START" && d.Status == "CREATED" || input.Command == "PAUSE" && d.Status == "RUNNING" || input.Command == "RESUME" && d.Status == "PAUSED" || input.Command == "END" && (d.Status == "CREATED" || d.Status == "RUNNING" || d.Status == "PAUSED")) {
			return out, ErrConflict
		}
	} else {
		return out, e
	}
	a.document = d
	a.raw, _ = json.Marshal(d)
	g, e := integrationControlGates(ctx, s.DB, d.Scope.ProductScope, a.clientControl(d.Scope.ProductScope, world))
	if e != nil {
		return out, e
	}
	r := BarrierRequest{integrationControl: a, ProductScope: d.Scope.ProductScope, WorldScope: &d.Scope, Versions: &d.Versions, OwnerSubjectID: d.Scope.SubjectID, Purpose: ControlPurpose, ExpectedWorldStatus: d.Status, Runtime: &RuntimeBinding{Action: d.Action}, Operation: "controlIntegrationInstance", CommandKey: key, CommitKey: d.Receipt, GateVersions: g, PayloadHash: a.hash, commandOnly: true, Locks: append(a.keys, LockKey{Rank: 50, IDs: []string{d.Scope.TenantID, d.Scope.ProductID, d.Task}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{d.Scope.TenantID, d.Scope.ProductID, d.Task}})}
	_, _, e = s.barrier(ctx, r, func(tx Tx) error {
		u, ok := tx.(*unit)
		if !ok || u.integrationControl != a {
			return ErrDenied
		}
		if err := u.CreateRegisteredTask(ctx, domain.TaskIntake{TaskScope: domain.TaskScope{ProductScope: d.Scope.ProductScope, WorldScope: &d.Scope, OwnerSubjectID: d.Scope.SubjectID}, TaskID: d.Task, Kind: "CONTROL", NextRunAt: time.Now().UTC()}, "INSTANCE_CONTROL"); err != nil {
			return err
		}
		if _, err := u.tx.ExecContext(ctx, "INSERT INTO runtime_tasks(tenant_id,product_id,task_id,document) VALUES(?,?,?,CAST(? AS JSON))", d.Scope.TenantID, d.Scope.ProductID, d.Task, string(a.raw)); err != nil {
			return err
		}
		return a.writeSidecar(ctx, u)
	})
	if e != nil {
		return out, e
	}
	out = generated.TaskReceipt{Scope: generated.WorldScope{TenantID: d.Scope.TenantID, ProductID: d.Scope.ProductID, WorldInstanceID: world}, TaskID: d.Task, TaskKind: "CONTROL", TaskState: "ACCEPTED", CorrelationID: d.Task}
	return out, nil
}
func integrationControlGates(ctx context.Context, q rowQuerier, p domain.ProductScope, privatePlayerControl bool) (GateVersions, error) {
	var g GateVersions
	var ok bool
	e := q.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,t.enabled AND p.enabled AND s.enabled AND (s.can_control OR ?) FROM subject_governance s JOIN product_gates p USING(tenant_id,product_id) JOIN tenant_gates t USING(tenant_id) WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", privatePlayerControl, p.TenantID, p.ProductID, p.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &ok)
	if e != nil || !ok {
		return g, ErrDenied
	}
	return g, nil
}
func (a *integrationControlPlan) writeSidecar(ctx context.Context, u *unit) error {
	d := a.document
	p := a.principal.principal
	if a.fresh != nil || u.tx == nil || u.world != d.Scope.WorldInstanceID || u.operatorStore != a.store || u.integrationControl != a || !u.commandOnly || u.operation != "controlIntegrationInstance" || u.scope != d.Scope.ProductScope {
		return ErrDenied
	}
	var now time.Time
	if e := u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return e
	}
	reg, _ := json.Marshal(a.registration())
	res, e := u.tx.ExecContext(ctx, `INSERT INTO integration_task_principals(tenant_id,product_id,task_id,task_kind,principal_kind,principal_ref,issuer_service_key,customer_id,player_subject_id,world_instance_id,action,accepted_at,issued_session_expires_at,original_execute_by,original_settle_by,intake_receipt,intake_hash,registration_revisions) VALUES(?,?,?,'CONTROL',?,?,?,?,?,?,?,?,?,?,?,?,?,CAST(? AS JSON))`, p.Tenant, p.Product, d.Task, p.Kind, p.PrincipalID, a.service.principal.PrincipalID, p.Customer, a.eligibility.entitlement.Player, d.Scope.WorldInstanceID, d.Action, now, p.ExpiresAt, now.Add(120*time.Second), now.Add(180*time.Second), d.Receipt, a.hash[:], string(reg))
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return ErrDenied
	}

	a.fresh = &integrationFreshWrite{store: a.store, unit: u, tx: u.tx, control: a, scope: d.Scope, task: d.Task, unitTask: u.taskID, operation: u.operation, command: d.Key, receipt: d.Receipt, kind: "CONTROL", principal: string(p.Kind), ref: p.PrincipalID, service: a.service.principal.PrincipalID, customer: p.Customer, player: a.eligibility.entitlement.Player, action: d.Action, hash: a.hash, registration: a.registration(), accepted: now, issued: p.ExpiresAt, execute: now.Add(120 * time.Second), settle: now.Add(180 * time.Second)}
	return nil
}

// Current authorization runs before original-result replay and after the final
// BeforeCommit hook. A historical COMMIT is identity evidence, never permission.
func (s *Store) checkIntegrationControl(ctx context.Context, q creationRows, r BarrierRequest) error {
	a := r.integrationControl
	if a == nil {
		return nil
	}
	if a.store != s || s.integrationClosed.Load() || s.Guard == nil || ctx.Value(operatorContextKey{}) != nil || r.Runtime == nil || r.Purpose != ControlPurpose || r.WorldScope == nil || *r.WorldScope != a.document.Scope || r.OwnerSubjectID != a.document.Scope.SubjectID || r.Runtime.Action != a.document.Action {
		return ErrDenied
	}
	proofCtx := ctx
	if a.lease != nil {
		if ctx.Value(integrationControlKey{}) != a || r.commandOnly || r.Operation != "INSTANCE_CONTROL" || r.TaskID != a.document.Task || r.Fence != a.lease.Fence || r.WorkerID != a.worker {
			return ErrDenied
		}
		proofCtx = context.WithValue(ctx, integrationContextKey{}, a.principal)
	} else {
		if !r.commandOnly || r.Operation != "controlIntegrationInstance" || r.CommandKey != a.document.Key || r.CommitKey != a.document.Receipt || r.PayloadHash != a.hash {
			return ErrDenied
		}
	}
	n, e := s.inspectIntegrationControl(proofCtx, q, a.principal, a.document.Scope.WorldInstanceID, a.document.Action, a.lease != nil)
	if e != nil || !reflect.DeepEqual(n.session, a.session) || !reflect.DeepEqual(n.eligibility, a.eligibility) || !reflect.DeepEqual(n.service, a.service) {
		return ErrDenied
	}
	// Subject control permission and all subject versions remain current at the
	// final owning gate, including mutations made by the BeforeCommit hook.
	g, e := integrationControlGates(ctx, q, r.ProductScope, a.clientControl(r.ProductScope, worldKey(r)))
	if e != nil || g != r.GateVersions {
		return ErrDenied
	}
	if a.fresh != nil {
		if e := a.fresh.check(ctx, q, r); e != nil {
			return e
		}
	}
	var exists bool
	if e = q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM runtime_tasks WHERE tenant_id=? AND product_id=? AND task_id=?)", a.document.Scope.TenantID, a.document.Scope.ProductID, a.document.Task).Scan(&exists); e != nil {
		return e
	}
	if exists {
		return a.checkOriginal(ctx, q, a.lease != nil)
	}
	if a.lease != nil {
		return ErrDenied
	}
	return nil
}
func (a *integrationControlPlan) checkOriginal(ctx context.Context, q creationRows, executing bool) error {
	d := a.document
	p := a.principal.principal
	if d.Task != creationID(d.Scope.TenantID, d.Scope.ProductID, d.Scope.SubjectID, d.Scope.WorldInstanceID, "controlIntegrationInstance", d.Key) || d.Receipt != creationID(d.Task, "accept") || d.Action != d.Request.Command {
		return ErrDenied
	}
	var raw, reg, hash []byte
	var kind, ref, service, customer, player, world, action, receipt string
	var issued, accepted, execute, settle time.Time
	var valid bool
	e := q.QueryRowContext(ctx, `SELECT r.document,i.principal_kind,i.principal_ref,i.issuer_service_key,i.customer_id,i.player_subject_id,i.world_instance_id,i.action,i.accepted_at,i.issued_session_expires_at,i.original_execute_by,i.original_settle_by,i.intake_receipt,i.intake_hash,i.registration_revisions,(i.task_kind='CONTROL' AND i.accepted_use_generation IS NULL AND i.accepted_use_expires_at IS NULL AND i.accepted_consent_revision IS NULL AND j.operation='INSTANCE_CONTROL' AND j.issuer_provenance='DIRECT_INTEGRATION' AND j.issuer_credential_id IS NULL AND t.task_kind='CONTROL' AND t.owner_subject_id=? AND t.scope_kind='WORLD' AND t.world_instance_id=i.world_instance_id) FROM integration_task_principals i JOIN runtime_tasks r USING(tenant_id,product_id,task_id) JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) WHERE i.tenant_id=? AND i.product_id=? AND i.task_id=?`, d.Scope.SubjectID, p.Tenant, p.Product, d.Task).Scan(&raw, &kind, &ref, &service, &customer, &player, &world, &action, &accepted, &issued, &execute, &settle, &receipt, &hash, &reg, &valid)
	var stored integrationControlDocument
	var registration integrationRegistrationProof
	if e != nil || !valid || json.Unmarshal(raw, &stored) != nil || !reflect.DeepEqual(stored, d) || json.Unmarshal(reg, &registration) != nil || registration != a.registration() || kind != string(p.Kind) || service != a.service.principal.PrincipalID || customer != p.Customer || player != a.eligibility.entitlement.Player || world != d.Scope.WorldInstanceID || action != d.Action || receipt != d.Receipt || !bytes.Equal(hash, a.hash[:]) || !execute.Equal(accepted.Add(120*time.Second)) || !settle.Equal(accepted.Add(180*time.Second)) {
		return ErrDenied
	}
	// Intake replays may use a new current Session for the same player. Original
	// issuance remains bound to the sidecar; workers must use that exact Session.
	if executing && (ref != p.PrincipalID || !issued.Equal(p.ExpiresAt)) {
		return ErrDenied
	}
	if !executing && kind == string(domain.IntegrationService) && ref != p.PrincipalID {
		return ErrDenied
	}
	if kind == string(domain.IntegrationClient) {
		var key string
		if q.QueryRowContext(ctx, "SELECT x.original_key FROM integration_principals p JOIN integration_receipts x ON x.receipt_ref=p.issuance_receipt AND x.tenant_id=p.tenant_id AND x.product_id=p.product_id AND x.owner_id=p.issuer_service_key AND x.operation='EXCHANGE_SESSION' WHERE p.principal_id=? AND p.kind='CLIENT'", ref).Scan(&key) != nil {
			return ErrDenied
		}
		original, err := sessionRead(ctx, q, a.service.principal, key)
		if err != nil || original.ID != ref || original.Player != d.Scope.SubjectID || original.World != world || !original.Expires.Equal(issued) || accepted.Before(original.Issued) || !accepted.Before(original.Expires) {
			return ErrDenied
		}
	}
	proof, e := queryReceipt(ctx, q, d.Receipt)
	if e != nil {
		return e
	}
	if !proof.matches(BarrierRequest{ProductScope: d.Scope.ProductScope, WorldScope: &d.Scope, OwnerSubjectID: d.Scope.SubjectID, Operation: "controlIntegrationInstance", CommandKey: d.Key, CommitKey: d.Receipt, PayloadHash: a.hash, commandOnly: true}) {
		return ErrDenied
	}
	if executing {
		var live bool
		e = q.QueryRowContext(ctx, "SELECT ?>UTC_TIMESTAMP(6) AND ?>UTC_TIMESTAMP(6)", execute, settle).Scan(&live)
		if e != nil || !live {
			return ErrDenied
		}
	}
	return nil
}

func (s *Store) acquireIntegrationControlKey(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) (bool, error) {
	a := r.integrationControl
	if a == nil {
		return false, nil
	}
	if k.Rank != 15 && k.Rank != 25 && k.Rank != 35 && !(k.Rank == 110 && k.Kind == "integration_receipt") {
		return false, nil
	}
	if a.store != s {
		return true, ErrDenied
	}
	found := false
	for _, x := range a.keys {
		if reflect.DeepEqual(x, k) {
			found = true
		}
	}
	if !found {
		return true, ErrDenied
	}
	if e := marker(ctx, tx, k); e != nil {
		return true, e
	}
	if k.Kind == "integration_participant" {
		return true, studioLockRows(ctx, tx, "SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
	}
	return true, lockStudioRows(ctx, tx, k)
}

// Only a real CONTROL lease selects this path. A direct HTTP context cannot be
// converted into Background authority, and an unknown provenance has no fallback.
func (s *Store) enterIntegrationControlIssuer(ctx context.Context, l RegisteredLease, worker string) (context.Context, error) {
	if s.integrationClosed.Load() {
		return ctx, ErrDenied
	}
	if l.Kind != "CONTROL" || l.Operation != "INSTANCE_CONTROL" || l.WorldInstanceID == nil || l.Fence == 0 || worker == "" || l.SubjectID != l.OwnerSubjectID || ctx.Value(integrationContextKey{}) != nil || ctx.Value(operatorContextKey{}) != nil {
		return ctx, ErrDenied
	}
	var id, action string
	if e := s.DB.QueryRowContext(ctx, "SELECT principal_ref,action FROM integration_task_principals WHERE tenant_id=? AND product_id=? AND task_id=? AND task_kind='CONTROL'", l.TenantID, l.ProductID, l.TaskID).Scan(&id, &action); e != nil {
		return ctx, ErrDenied
	}
	v := currentIntegration{store: s}
	p := &v.principal
	var hash []byte
	if e := s.DB.QueryRowContext(ctx, "SELECT principal_id,kind,tenant_id,product_id,customer_id,subject_id,COALESCE(world_instance_id,''),profile,expires_at,token_hash FROM integration_principals WHERE principal_id=?", id).Scan(&p.PrincipalID, &p.Kind, &p.Tenant, &p.Product, &p.Customer, &p.StableSubject, &p.World, &p.Profile, &p.ExpiresAt, &hash); e != nil || len(hash) != 32 || p.Tenant != l.TenantID || p.Product != l.ProductID || p.StableSubject != l.OwnerSubjectID {
		return ctx, ErrDenied
	}
	copy(v.hash[:], hash)
	proofCtx := context.WithValue(ctx, integrationContextKey{}, v)
	a, e := s.inspectIntegrationControl(proofCtx, s.DB, v, *l.WorldInstanceID, action, true)
	if e != nil {
		return ctx, e
	}
	if e = s.DB.QueryRowContext(ctx, "SELECT document FROM runtime_tasks WHERE tenant_id=? AND product_id=? AND task_id=?", l.TenantID, l.ProductID, l.TaskID).Scan(&a.raw); e != nil || json.Unmarshal(a.raw, &a.document) != nil {
		return ctx, ErrDenied
	}
	a.hash, e = integrationControlHash(a.document)
	if e != nil || a.document.Task != l.TaskID || a.document.Scope.ProductScope != l.ProductScope || a.document.Scope.WorldInstanceID != *l.WorldInstanceID {
		return ctx, ErrDenied
	}
	a.lease = &l
	a.worker = worker
	a.keys = append(a.keys, LockKey{Rank: 40, IDs: []string{l.TenantID, l.ProductID, *l.WorldInstanceID}}, harnessTaskKey(l.ProductScope, l.TaskID), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{a.document.Receipt}})
	tx, e := s.integrationMemoryReadTx(ctx, a.keys)
	if e != nil {
		return ctx, e
	}
	defer tx.Rollback()
	check := func() error {
		n, err := s.inspectIntegrationControl(proofCtx, tx, v, *l.WorldInstanceID, action, true)
		if err != nil || !reflect.DeepEqual(n.session, a.session) || !reflect.DeepEqual(n.eligibility, a.eligibility) || !reflect.DeepEqual(n.service, a.service) || s.integrationClosed.Load() {
			return ErrDenied
		}
		if err = a.checkOriginal(ctx, tx, true); err != nil {
			return err
		}
		var live bool
		err = tx.QueryRowContext(ctx, "SELECT state='RUNNING' AND fence=? AND lease_owner=? AND lease_expires_at>UTC_TIMESTAMP(6) AND cancelled=0 FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=?", l.Fence, worker, l.TenantID, l.ProductID, l.TaskID).Scan(&live)
		if err != nil || !live {
			return ErrDenied
		}
		return nil
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
	return context.WithValue(ctx, integrationControlKey{}, a), nil
}
