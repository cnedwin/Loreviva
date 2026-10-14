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
	"strconv"
	"time"
	"unicode/utf8"
)

// Mock text metering counts Unicode code points, deterministically. These
// finite descriptor bounds apply before intake and again to the assembled send.
const MockInputLimit = 16000
const MockOutputLimit = 4000

type TurnInputOwner interface {
	ResolveTurnRouteTx(context.Context, Tx, domain.Scope, string) (domain.TurnRouteAuthority, error)
	AcceptTurnInputTx(context.Context, Tx, domain.TurnInput) error
}

func (u *unit) AssertTurnIntake(s domain.Scope, id string) error {
	if !u.commandOnly || u.acceptedTurnID == "" || u.acceptedTurnID != id || u.scope != s.ProductScope || u.world != s.WorldInstanceID || u.purpose != ExecutionPurpose {
		u.err = ErrDenied
		return u.err
	}
	return nil
}
func turnIdentity(parts ...string) string {
	_, hash, _ := harnessPayload(parts)
	return hex.EncodeToString(hash[:])
}
func turnScope(s domain.Scope) generated.WorldScope {
	return generated.WorldScope{TenantID: s.TenantID, ProductID: s.ProductID, WorldInstanceID: s.WorldInstanceID}
}
func r0Owner(s domain.Scope) bool {
	return s.Validate() == nil && s.TenantID == "demo-local" && s.ProductID == "coffee" && s.SubjectID == "creator"
}

// Isolation is a server-owned immutable world binding, not the public PRIVATE
// mode. Ordinary task metadata and intake never consume the rehearsal bridge.
func (s *Store) ordinaryTurnScope(ctx context.Context, scope domain.Scope) error {
	if scope.Validate() != nil {
		return ErrDenied
	}
	var isolated bool
	if e := s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_rehearsal_worlds WHERE tenant_id=? AND product_id=? AND world_instance_id=?)", scope.TenantID, scope.ProductID, scope.WorldInstanceID).Scan(&isolated); e != nil {
		return e
	}
	if isolated {
		return ErrDenied
	}
	return nil
}
func typedJSON(schema string, v any) error {
	raw, e := json.Marshal(v)
	if e != nil {
		return e
	}
	var out any
	return generated.Decode(schema, raw, &out)
}

func (s *Store) QuoteTurn(ctx context.Context, c CommandBarrierRequest, request generated.QuoteRequest) (generated.Quote, error) {
	var out generated.Quote
	if c.WorldScope == nil || !r0Owner(*c.WorldScope) || c.Purpose != ExecutionPurpose || s.TurnOwner == nil || request.PrimaryActorID == nil || request.Scope != turnScope(*c.WorldScope) || request.ServiceKind != "TURN_TEXT" || request.PurchaseResponsibility != "MOCK" {
		return out, ErrDenied
	}
	if e := s.ordinaryTurnScope(ctx, *c.WorldScope); e != nil {
		return out, e
	}
	if e := typedJSON("QuoteRequest", request); e != nil {
		return out, e
	}
	input, e := strconv.ParseUint(request.InputUpperBound, 10, 64)
	if e != nil || input == 0 || input > MockInputLimit {
		return out, ErrDenied
	}
	output, e := strconv.ParseUint(request.OutputUpperBound, 10, 64)
	if e != nil || output == 0 || output > MockOutputLimit {
		return out, ErrDenied
	}
	_, hash, e := harnessPayload(struct {
		Scope   domain.Scope
		Request generated.QuoteRequest
	}{*c.WorldScope, request})
	if e != nil {
		return out, e
	}
	id := turnIdentity(c.ProductScope.TenantID, c.ProductScope.ProductID, c.OwnerSubjectID, c.WorldScope.WorldInstanceID, "TURN_QUOTE", c.CommandKey)
	r := harnessCommand(c)
	r.Operation = "TURN_QUOTE"
	r.CommitKey = turnIdentity(id, "receipt")
	r.PayloadHash = hash
	r.Locks = append(r.Locks, harnessTaskKey(c.ProductScope, id))
	r.Locks = append(r.Locks, NewRouteAdmissionLocks(c.ProductScope)...)
	if e = s.planPerception(ctx, &r, *request.PrimaryActorID); e != nil {
		return out, e
	}
	_, _, e = s.barrier(ctx, r, func(port Tx) error {
		u := port.(*unit)
		if e := CheckNewRouteAdmissionTx(ctx, port, c.ProductScope, domain.InstalledMockConnection, domain.InstalledMockModel); e != nil {
			return e
		}
		route, e := s.TurnOwner.ResolveTurnRouteTx(ctx, u, *c.WorldScope, *request.PrimaryActorID)
		if e != nil {
			return e
		}
		var rh [32]byte
		b, e := hex.DecodeString(route.RouteID)
		if e != nil || len(b) != 32 {
			return ErrDenied
		}
		copy(rh[:], b)
		q := HarnessQuote{ID: id, ProjectID: route.ProjectID, Kind: "TURN", RequestHash: hash, RouteHash: rh}
		raw, _, e := harnessPayload(q)
		if e != nil {
			return e
		}
		var now time.Time
		if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return e
		}
		expiry := now.Add(time.Minute)
		out = generated.Quote{QuoteID: id, Scope: request.Scope, RouteID: route.RouteID, PurchaseResponsibility: "MOCK", RateBookVersion: "r0-mock-scale8-v1", ContractReference: "r0-bounded-turn-v1", ServiceUnit: "WE_CREDIT", Lines: []generated.QuoteLine{{SKU: "TURN_TEXT", Meter: "INPUT_TOKEN", IncludedUnits: "0", MaxMeterUnits: request.InputUpperBound, MaxServiceUnits: "0", RateUnits: "0", CompletionCondition: "APPROVED_RESULT_RETRIEVABLE"}, {SKU: "TURN_TEXT", Meter: "OUTPUT_TOKEN", IncludedUnits: "0", MaxMeterUnits: request.OutputUpperBound, MaxServiceUnits: "0", RateUnits: "0", CompletionCondition: "APPROVED_RESULT_RETRIEVABLE"}}, EstimatedServiceUnits: "0", MaxServiceUnits: "0", SupplierCostCapUnits: "0", SupplierCostCurrency: "USD", Rounding: "SUM_THEN_HALF_EVEN", ExpiresAt: expiry.Format(time.RFC3339Nano), ReservationSettleBy: expiry.Add(180 * time.Second).Format(time.RFC3339Nano), RiskBearer: "PLATFORM", CommercialStatus: "SYNTHETIC_MOCK"}
		if e = typedJSON("Quote", out); e != nil {
			return e
		}
		public, _ := json.Marshal(out)
		if _, e = u.tx.ExecContext(ctx, "INSERT INTO harness_quotes(tenant_id,product_id,quote_id,owner_subject_id,world_instance_id,project_id,document,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)", c.ProductScope.TenantID, c.ProductScope.ProductID, id, c.OwnerSubjectID, c.WorldScope.WorldInstanceID, route.ProjectID, raw, now, expiry); e != nil {
			return e
		}
		_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_turn_quotes VALUES(?,?,?,?,?,?,?)", c.ProductScope.TenantID, c.ProductScope.ProductID, id, route.ActorID, input, output, public)
		return e
	})
	if e != nil {
		return generated.Quote{}, e
	}
	var raw []byte
	e = s.DB.QueryRowContext(ctx, "SELECT public_document FROM harness_turn_quotes WHERE tenant_id=? AND product_id=? AND quote_id=?", c.ProductScope.TenantID, c.ProductScope.ProductID, id).Scan(&raw)
	if e == nil {
		e = generated.Decode("Quote", raw, &out)
	}
	return out, e
}

type turnIntake struct {
	Request                   generated.TurnRequest
	Hash, InputHash           [32]byte
	Body, SourceID, ProjectID string
}

func TurnCommandHash(scope domain.Scope, request generated.TurnRequest) ([32]byte, error) {
	_, hash, e := harnessPayload(struct {
		Scope   domain.Scope
		Command generated.TurnRequest
	}{scope, request})
	return hash, e
}

// An exact original acceptance is returned before the new-intake revision comparison; its current scope gates and command hash still have to match.
func (s *Store) AcceptR0Turn(ctx context.Context, c CommandBarrierRequest, request generated.TurnRequest) (generated.TurnAccepted, error) {
	var out generated.TurnAccepted
	if c.WorldScope == nil || !r0Owner(*c.WorldScope) || s.TurnOwner == nil || request.RequestedAudience != "PRIVATE_RELATIONSHIP" {
		return out, ErrDenied
	}
	if e := typedJSON("TurnRequest", request); e != nil {
		return out, e
	}
	hash, e := TurnCommandHash(*c.WorldScope, request)
	if e != nil {
		return out, e
	}
	if original, e := s.OriginalTurn(ctx, *c.WorldScope, c.CommandKey, hash); e == nil {
		return original, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return out, e
	}
	if c.Versions == nil || request.ExpectedVersions.WorldRevision != strconv.FormatUint(c.Versions.WorldRevision, 10) || request.ExpectedVersions.ControlVersion != strconv.FormatUint(c.Versions.ControlVersion, 10) {
		return out, ErrConflict
	}
	var raw []byte
	var actor string
	var input, output uint64
	e = s.DB.QueryRowContext(ctx, "SELECT q.document,t.actor_id,t.input_bound,t.output_bound FROM harness_quotes q JOIN harness_turn_quotes t ON t.tenant_id=q.tenant_id AND t.product_id=q.product_id AND t.quote_id=q.quote_id WHERE q.tenant_id=? AND q.product_id=? AND q.quote_id=? AND q.owner_subject_id=? AND q.world_instance_id=?", c.ProductScope.TenantID, c.ProductScope.ProductID, request.QuoteID, c.OwnerSubjectID, c.WorldScope.WorldInstanceID).Scan(&raw, &actor, &input, &output)
	if e != nil {
		return out, e
	}
	if actor != request.PrimaryActorID || input > MockInputLimit || output > MockOutputLimit {
		return out, ErrDenied
	}
	canonical, e := wire.Canonical(request.Input)
	if e != nil || uint64(utf8.RuneCount(canonical)) > input {
		return out, ErrDenied
	}
	var quote HarnessQuote
	if e = json.Unmarshal(raw, &quote); e != nil {
		return out, e
	}
	if quote.ServiceAtoms != 0 || quote.SupplierAtoms != 0 {
		return out, ErrDenied
	}
	id := turnIdentity(c.ProductScope.TenantID, c.ProductScope.ProductID, c.OwnerSubjectID, c.WorldScope.WorldInstanceID, "TURN", c.CommandKey)
	in := HarnessIntake{TaskID: id, QuoteID: quote.ID, RequestHash: quote.RequestHash, RouteHash: quote.RouteHash, ParentHolds: r0ParentHolds(*c.WorldScope, quote.ProjectID, id)}
	turn := &turnIntake{Request: request, Hash: hash, InputHash: sha256.Sum256(canonical), Body: string(canonical), SourceID: turnIdentity(id, "input"), ProjectID: quote.ProjectID}
	c.Operation = "HARNESS_INTAKE"
	c.CommitKey = turnIdentity(id, "accepted")
	if _, e = s.acceptHarness(ctx, c, in, turn); e != nil {
		return out, e
	}
	return s.OriginalTurn(ctx, *c.WorldScope, c.CommandKey, hash)
}

func (s *Store) OriginalTurn(ctx context.Context, scope domain.Scope, key string, hash [32]byte) (generated.TurnAccepted, error) {
	if e := s.ordinaryTurnScope(ctx, scope); e != nil {
		return generated.TurnAccepted{}, e
	}
	var out generated.TurnAccepted
	if scope.Validate() != nil || domain.ValidateIdempotencyKey(key) != nil {
		return out, ErrDenied
	}
	var id, quote, state string
	var stored []byte
	e := s.DB.QueryRowContext(ctx, "SELECT h.task_id,h.quote_id,t.state,h.command_hash FROM harness_turns h JOIN tasks t ON t.tenant_id=h.tenant_id AND t.product_id=h.product_id AND t.task_id=h.task_id JOIN commit_receipts c ON c.commit_key=h.accepted_commit_key AND c.tenant_id=h.tenant_id AND c.product_id=h.product_id AND c.owner_subject_id=h.owner_subject_id AND c.world_instance_id=h.world_instance_id AND c.command_key=h.original_key AND c.payload_hash=h.command_hash AND c.operation='HARNESS_INTAKE' AND c.phase='COMMIT' JOIN tenant_gates tg ON tg.tenant_id=t.tenant_id JOIN product_gates pg ON pg.tenant_id=t.tenant_id AND pg.product_id=t.product_id JOIN subject_governance g ON g.tenant_id=t.tenant_id AND g.product_id=t.product_id AND g.subject_id=t.owner_subject_id WHERE h.tenant_id=? AND h.product_id=? AND h.world_instance_id=? AND h.owner_subject_id=? AND h.original_key=? AND t.owner_subject_id=? AND t.world_instance_id=? AND tg.enabled=1 AND pg.enabled=1 AND g.enabled=1 AND g.can_execute=1", scope.TenantID, scope.ProductID, scope.WorldInstanceID, scope.SubjectID, key, scope.SubjectID, scope.WorldInstanceID).Scan(&id, &quote, &state, &stored)
	if e != nil {
		return out, e
	}
	if hex.EncodeToString(stored) != hex.EncodeToString(hash[:]) {
		return out, ErrConflict
	}
	// Original acceptance is immutable even when current task state changes.
	return generated.TurnAccepted{TurnID: id, QuoteID: quote, Task: generated.TaskReceipt{TaskID: id, TaskKind: "TURN", TaskState: "ACCEPTED", Scope: turnScope(scope), CorrelationID: id}}, nil
}

func r0Accounts(s domain.Scope, project string) []Reservation {
	product := s.TenantID + "." + s.ProductID
	return []Reservation{{Kind: "supplier_global", ScopeKey: "GLOBAL", AccountID: "r0-mock-supplier"}, {Kind: "supplier_tenant", ScopeKey: s.TenantID, AccountID: "r0-mock-supplier"}, {Kind: "platform_risk", ScopeKey: "GLOBAL", AccountID: "r0-mock-risk"}, {Kind: "customer", ScopeKey: product, AccountID: "r0-owner"}, {Kind: "project", ScopeKey: product, AccountID: project}, {Kind: "world", ScopeKey: product + "." + s.WorldInstanceID, AccountID: s.WorldInstanceID}}
}
func r0ParentHolds(s domain.Scope, project, task string) []Reservation {
	out := []Reservation{}
	for _, a := range r0Accounts(s, project) {
		if a.Kind == "supplier_global" || a.Kind == "supplier_tenant" {
			continue
		}
		a.ReservationID = turnIdentity(task, "parent", a.Kind)
		out = append(out, a)
	}
	return out
}

// R0 account provisioning is limited to the owning intake plan, current generation consent and the quote's bound project; these inserted accounts start with zero balance and held amounts.
func provisionR0TurnAccount(ctx context.Context, tx *sql.Tx, r BarrierRequest, k LockKey) error {
	if !r.r0TurnProvision || !r.commandOnly || r.WorldScope == nil || !r0Owner(*r.WorldScope) || r.Operation != "HARNESS_INTAKE" {
		return ErrDenied
	}
	var project string
	var valid bool
	if e := tx.QueryRowContext(ctx, "SELECT q.project_id,EXISTS(SELECT 1 FROM r0_generation_consent c JOIN operator_credentials o ON o.credential_id=c.credential_id JOIN operator_actions a ON a.credential_id=o.credential_id LEFT JOIN studio_credential_state v ON v.credential_id=o.credential_id WHERE c.tenant_id=q.tenant_id AND c.product_id=q.product_id AND c.subject_id=q.owner_subject_id AND c.opted_in=1 AND c.expires_at>UTC_TIMESTAMP(6) AND (v.expires_at IS NULL OR v.expires_at>UTC_TIMESTAMP(6)) AND o.tenant_id=q.tenant_id AND o.product_id=q.product_id AND o.subject_id=q.owner_subject_id AND o.enabled=1 AND a.action='memory.generation.consent') FROM harness_quotes q JOIN runtime_instances i ON i.tenant_id=q.tenant_id AND i.product_id=q.product_id AND i.world_instance_id=q.world_instance_id AND i.owner_subject_id=q.owner_subject_id WHERE q.tenant_id=? AND q.product_id=? AND q.quote_id=? AND q.owner_subject_id=? AND q.world_instance_id=? AND q.project_id=JSON_UNQUOTE(JSON_EXTRACT(CONVERT(i.initialization USING utf8mb4),'$.ProjectID'))", r.ProductScope.TenantID, r.ProductScope.ProductID, r.harnessIntakeQuote, r.OwnerSubjectID, r.WorldScope.WorldInstanceID).Scan(&project, &valid); e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	found := false
	for _, a := range r0Accounts(*r.WorldScope, project) {
		if a.Kind == k.Kind && a.ScopeKey == k.IDs[0] && a.AccountID == k.IDs[1] {
			found = true
		}
	}
	if !found {
		return ErrDenied
	}
	var tenant, productID, worldID, projectID any
	if k.Kind != "supplier_global" && k.Kind != "platform_risk" {
		tenant = r.ProductScope.TenantID
		if k.Kind != "supplier_tenant" {
			productID = r.ProductScope.ProductID
		}
	}
	if k.Kind == "world" {
		worldID = r.WorldScope.WorldInstanceID
	}
	if k.Kind == "project" {
		projectID = project
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO budget_accounts(kind,scope_key,account_id,tenant_id,product_id,world_instance_id,project_id,balance,held) VALUES(?,?,?,?,?,?,?,0,0)", k.Kind, k.IDs[0], k.IDs[1], tenant, productID, worldID, projectID)
	return e
}

// ReadTurn projects only the original owner's task metadata. No source body is
// reconstructed by a read or replay; narrative packets require their own gate.
func (s *Store) ReadTurn(ctx context.Context, scope domain.Scope, id string) (generated.TurnView, error) {
	out := generated.TurnView{ApprovedPackets: []generated.ApprovedPacket{}, Attempts: []generated.AttemptSummary{}, Deliveries: []generated.DeliveryUpdate{}}
	if scope.Validate() != nil || domain.ValidateID(id) != nil {
		return out, ErrDenied
	}
	if e := s.ordinaryTurnScope(ctx, scope); e != nil {
		return out, e
	}
	var state string
	var settle time.Time
	var v domain.Versions
	e := s.DB.QueryRowContext(ctx, "SELECT t.state,e.settle_by,w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch FROM harness_turns h JOIN tasks t ON t.tenant_id=h.tenant_id AND t.product_id=h.product_id AND t.task_id=h.task_id JOIN harness_envelopes e ON e.tenant_id=t.tenant_id AND e.product_id=t.product_id AND e.task_id=t.task_id JOIN world_controls w ON w.tenant_id=t.tenant_id AND w.product_id=t.product_id AND w.world_instance_id=t.world_instance_id JOIN subject_governance g ON g.tenant_id=t.tenant_id AND g.product_id=t.product_id AND g.subject_id=t.owner_subject_id JOIN tenant_gates tg ON tg.tenant_id=t.tenant_id JOIN product_gates pg ON pg.tenant_id=t.tenant_id AND pg.product_id=t.product_id WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=? AND t.owner_subject_id=? AND t.world_instance_id=? AND tg.enabled=1 AND pg.enabled=1 AND g.enabled=1 AND g.can_execute=1", scope.TenantID, scope.ProductID, id, scope.WorldInstanceID, scope.SubjectID, scope.SubjectID, scope.WorldInstanceID).Scan(&state, &settle, &v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch)
	if e != nil {
		return out, e
	}
	out.TurnID = id
	out.Task = generated.TaskReceipt{TaskID: id, TaskKind: "TURN", TaskState: state, Scope: turnScope(scope), CorrelationID: id}
	out.Versions = generated.Versions{ReleaseID: v.ReleaseID, WorldRevision: strconv.FormatUint(v.WorldRevision, 10), ControlVersion: strconv.FormatUint(v.ControlVersion, 10), PrivacyEpoch: strconv.FormatUint(v.PrivacyEpoch, 10), ViewEpoch: strconv.FormatUint(v.ViewEpoch, 10)}
	deadline := settle.Format(time.RFC3339Nano)
	out.Billing = generated.BillingSummary{CustomerConsumedUnits: "0", CustomerReservedUnits: "0", ReservationSettleBy: &deadline, RiskBearer: "PLATFORM", ServiceSettlementState: "RESERVED", ServiceUnit: "WE_CREDIT", SupplierCostCurrency: "USD", SupplierSafetyReservedUnits: "0"}
	if e = s.projectTurnSettlement(ctx, scope, id, &out); e != nil {
		return out, e
	}
	rows, e := s.DB.QueryContext(ctx, "SELECT a.attempt_id,a.state,a.route,q.attempt_id IS NOT NULL,COALESCE(q.provider_state='SUCCEEDED' AND JSON_EXTRACT(q.metadata,'$.UsageKnown')=true,0) FROM attempts a LEFT JOIN harness_original_queries q USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot", scope.TenantID, scope.ProductID, id)
	if e != nil {
		return out, e
	}
	defer rows.Close()
	for rows.Next() {
		var attempt generated.AttemptSummary
		var query, known bool
		if e = rows.Scan(&attempt.AttemptID, &attempt.AttemptState, &attempt.RouteID, &query, &known); e != nil {
			return out, e
		}
		attempt.PurchaseResponsibility = "MOCK"
		attempt.QueryCapability = "UNSUPPORTED"
		attempt.CancelState = "NOT_REQUESTED"
		attempt.UsageState = "UNKNOWN"
		if query {
			attempt.QueryCapability = "SUPPORTED"
		}
		if !known {
			known, e = originalMeterUsageKnown(ctx, s.DB, scope, id, string(attempt.AttemptID))
			if e != nil {
				return out, e
			}
		}
		if known {
			attempt.UsageState = "AUTHORITATIVE"
		}
		out.Attempts = append(out.Attempts, attempt)
	}
	if e = rows.Err(); e != nil {
		return out, e
	}
	return out, typedJSON("TurnView", out)
}

// OriginalTurnDispatch derives the first request entirely from accepted metadata.
// The returned value is not authorization: both purchase boundaries revalidate
// current context, holds, lease and original identity under the closed barrier.
func (s *Store) OriginalTurnDispatch(ctx context.Context, r BarrierRequest) (DispatchRequest, error) {
	var out DispatchRequest
	// Isolated rehearsal uses its private, lease-bound owning plan. The online
	// R0 turn path retains its original owner-only policy.
	if r.WorldScope == nil || (r.rehearsalExecution == nil && !r0Owner(*r.WorldScope)) || r.TaskID == "" {
		return out, ErrDenied
	}
	var quote, project string
	var raw []byte
	e := s.DB.QueryRowContext(ctx, "SELECT h.quote_id,q.project_id,b.document FROM harness_turns h JOIN harness_quotes q ON q.tenant_id=h.tenant_id AND q.product_id=h.product_id AND q.quote_id=h.quote_id JOIN harness_context_bindings b ON b.tenant_id=h.tenant_id AND b.product_id=h.product_id AND b.task_id=h.task_id WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=?", r.ProductScope.TenantID, r.ProductScope.ProductID, r.TaskID, r.WorldScope.WorldInstanceID, r.OwnerSubjectID).Scan(&quote, &project, &raw)
	if e != nil {
		return out, e
	}
	var binding domain.GenerationBinding
	if e = json.Unmarshal(raw, &binding); e != nil {
		return out, e
	}
	if binding.InputSourceID == "" || binding.TurnCommandHash == "" {
		return out, ErrDenied
	}
	attempt := turnIdentity(r.TaskID, "attempt", "0")
	r.Operation = "TURN_DISPATCH"
	r.CommandKey = turnIdentity(r.TaskID, "authorize")
	r.CommitKey = r.CommandKey
	out = DispatchRequest{BarrierRequest: r, HarnessQuoteID: quote, AttemptID: attempt, AuthorizationID: turnIdentity(attempt, "authorization"), Route: binding.RouteID, RequestKey: turnIdentity(attempt, "request")}
	hash, e := hex.DecodeString(binding.RequestHash)
	if e != nil || len(hash) != 32 {
		return DispatchRequest{}, ErrDenied
	}
	copy(out.RequestHash[:], hash)
	for _, a := range r0Accounts(*r.WorldScope, project) {
		if a.Kind != "supplier_global" && a.Kind != "supplier_tenant" {
			continue
		}
		a.ReservationID = turnIdentity(attempt, "supplier", a.Kind)
		out.Reservations = append(out.Reservations, a)
	}
	return out, nil
}

// projectTurnSettlement never turns a mere timeout or missing receipt into a
// failed narrative. Only the complete immutable cutoff proof can do so.
func (s *Store) projectTurnSettlement(ctx context.Context, scope domain.Scope, task string, out *generated.TurnView) error {
	return projectTurnSettlementTx(ctx, s.DB, scope, task, out)
}
func projectTurnSettlementTx(ctx context.Context, q rowQuerier, scope domain.Scope, task string, out *generated.TurnView) error {
	var state string
	if e := q.QueryRowContext(ctx, "SELECT state FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&state); e != nil {
		return e
	}
	worldKey := turnIdentity(task, "world-commit")
	world, e := queryReceipt(ctx, q, worldKey)
	if e == nil {
		var key string
		if e = q.QueryRowContext(ctx, "SELECT receipt_key FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&key); e != nil {
			return e
		}
		if key != worldKey || !settlementReceiptScope(world, scope, task) || world.operation != "HARNESS_TURN_COMMIT" {
			return ErrConflict
		}
		out.Task.TaskState = "COMMITTED"
		var meter, status, original string
		if e = q.QueryRowContext(ctx, "SELECT m.state,s.status,m.business_receipt FROM harness_service_metering m JOIN service_events s ON s.tenant_id=m.tenant_id AND s.product_id=m.product_id AND s.record_id=m.service_id WHERE m.tenant_id=? AND m.product_id=? AND m.task_id=? AND s.task_id=m.task_id AND s.owner_subject_id=?", scope.TenantID, scope.ProductID, task, scope.SubjectID).Scan(&meter, &status, &original); e != nil {
			return e
		}
		if original != worldKey {
			return ErrConflict
		}
		switch {
		case meter == "SETTLED" && status == "COMMITTED":
			out.Billing.ServiceSettlementState = "SETTLED"
		case meter == "PENDING_VERIFICATION" && status == "PENDING":
			out.Billing.ServiceSettlementState = "PENDING_VERIFICATION"
		case meter == "RELEASED" && status == "RELEASED":
			var raw, hash []byte
			var service string
			if e = q.QueryRowContext(ctx, "SELECT service_id,original_hash,disposition_proof FROM harness_service_metering WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&service, &hash, &raw); e != nil {
				return e
			}
			disposition, e := checkMeterReleaseTx(ctx, q, scope.ProductScope, task, service, original, hash, raw)
			if e != nil {
				return e
			}
			out.Billing.ServiceSettlementState = disposition
		default:
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if state != "CUTOFF" && state != "ACCOUNTING_ONLY" {
		if out.Task.TaskState == "COMMITTED" {
			return ErrConflict
		}
		return nil
	}
	var raw []byte
	var key string
	if e = q.QueryRowContext(ctx, "SELECT proof,receipt_key FROM harness_cutoffs WHERE tenant_id=? AND product_id=? AND task_id=?", scope.TenantID, scope.ProductID, task).Scan(&raw, &key); e != nil {
		return e
	}
	var proof cutoffProof
	if json.Unmarshal(raw, &proof) != nil || proof.Scope != scope || proof.Task != task || proof.WorldReceipt != worldKey || proof.CheckedAt.Before(proof.SettleBy) || proof.CheckedAt.Before(proof.ExecuteBy) {
		return ErrConflict
	}
	receipt, e := queryReceipt(ctx, q, key)
	if e != nil {
		return e
	}
	_, hash, e := harnessPayload(proof)
	if e != nil {
		return e
	}
	if !settlementReceiptScope(receipt, scope, task) || receipt.operation != "HARNESS_CUTOFF" || !bytes.Equal(receipt.hash, hash[:]) {
		return ErrConflict
	}
	if out.Task.TaskState != "CANCELLED" && out.Task.TaskState != "REVOKED" {
		out.Task.TaskState = "FAILED"
	}
	out.Billing.ServiceSettlementState = "ADJUSTED"
	for _, attempt := range proof.Attempts {
		if attempt.Marker != "" {
			out.Billing.ServiceSettlementState = "RISK_TRANSFERRED"
			break
		}
	}
	return nil
}
