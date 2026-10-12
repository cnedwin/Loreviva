package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"time"
)

type OriginalQueryAuthority interface {
	EnterOriginalQuery(context.Context) (context.Context, domain.QueryServiceIdentity, func(), error)
}
type OriginalProviderEvidence interface {
	OriginalMetadata(context.Context, domain.MockRequestIdentity) (domain.MockResultMetadata, error)
}
type OriginalQueryLease struct {
	ioPermit *queryIOPermit
	queryKey string
	cost     bool
	reply    bool
	identity domain.MockRequestIdentity
	service  domain.QueryServiceIdentity
	fence    uint64
	owner    string
}

func (l OriginalQueryLease) Identity() domain.MockRequestIdentity { return l.identity }

// The closed cost path takes the same real rows in canonical rank order, while
// deliberately not interpreting ordinary execution status as cost authority.
func lockOriginalQuery(ctx context.Context, tx *sql.Tx, id domain.MockRequestIdentity, service domain.QueryServiceIdentity) error {
	if id.Validate() != nil {
		return ErrDenied
	}
	t, p, w, sub := id.Scope.TenantID, id.Scope.ProductID, id.Scope.WorldInstanceID, id.Scope.SubjectID
	var found string
	if e := tx.QueryRowContext(ctx, "SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE", t).Scan(&found); e != nil {
		return e
	}
	if e := tx.QueryRowContext(ctx, "SELECT product_id FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE", t, p).Scan(&found); e != nil {
		return e
	}
	if e := queryServiceTx(ctx, tx, service); e != nil {
		return e
	}
	if e := tx.QueryRowContext(ctx, "SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE", t, p, sub).Scan(&found); e != nil {
		return e
	}
	if id.Kind == "" {
		if e := tx.QueryRowContext(ctx, "SELECT world_instance_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", t, p, w).Scan(&found); e != nil {
			return e
		}
	}
	var owner string
	var world sql.NullString
	if e := tx.QueryRowContext(ctx, "SELECT owner_subject_id,world_instance_id FROM tasks WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", t, p, id.TaskID).Scan(&owner, &world); e != nil {
		return e
	}
	if owner != sub || world.String != w || (id.Kind == "CANDIDATE" && world.Valid) || (id.Kind == "" && !world.Valid) {
		return ErrDenied
	}
	if id.Kind == "CANDIDATE" {
		var valid bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_admin_candidates c JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN tasks t USING(tenant_id,product_id,task_id) WHERE c.tenant_id=? AND c.product_id=? AND c.task_id=? AND c.owner_subject_id=? AND j.operation='CANDIDATE' AND t.task_kind='ADMIN')", t, p, id.TaskID, sub).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return ErrDenied
		}
	}
	if e := tx.QueryRowContext(ctx, "SELECT quote_id FROM harness_envelopes WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", t, p, id.TaskID).Scan(&found); e != nil {
		return e
	}
	if found != id.QuoteID {
		return ErrDenied
	}
	if e := marker(ctx, tx, LockKey{Rank: 60, IDs: []string{t, p, id.TaskID, id.AttemptID, id.AuthorizationID}}); e != nil {
		return e
	}
	var request, route, mark string
	var hash []byte
	if e := tx.QueryRowContext(ctx, "SELECT request_key,route,request_hash,marker_commit_key FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND send_started_at IS NOT NULL FOR UPDATE", t, p, id.TaskID, id.AttemptID).Scan(&request, &route, &hash, &mark); e != nil {
		return e
	}
	if request != id.RequestKey || route != id.RouteID || hex.EncodeToString(hash) != id.RequestHash || mark != id.MarkerKey {
		return ErrDenied
	}
	if e := tx.QueryRowContext(ctx, "SELECT owner_subject_id FROM dispatch_authorizations WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND authorization_id=? FOR UPDATE", t, p, id.TaskID, id.AttemptID, id.AuthorizationID).Scan(&owner); e != nil {
		return e
	}
	if owner != sub {
		return ErrDenied
	}
	var raw []byte
	var installation, serviceID, policy string
	if e := tx.QueryRowContext(ctx, "SELECT request_identity,installation_id,service_id,policy FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? FOR UPDATE", t, p, id.TaskID, id.AttemptID).Scan(&raw, &installation, &serviceID, &policy); e != nil {
		return e
	}
	var original domain.MockRequestIdentity
	if json.Unmarshal(raw, &original) != nil || original != id || installation != service.Installation || serviceID != service.ServiceID || policy != service.Policy {
		return ErrDenied
	}
	var valid bool
	var receiptWorld any = w
	if id.Kind == "CANDIDATE" {
		receiptWorld = nil
	}
	if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM commit_receipts WHERE commit_key=? AND tenant_id=? AND product_id=? AND task_id=? AND owner_subject_id=? AND world_instance_id<=>? AND phase='SEND')", id.MarkerKey, t, p, id.TaskID, sub, receiptWorld).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}

func (s *Store) turnQueryAdmission(ctx context.Context, d DispatchRequest) (context.Context, *domain.QueryServiceIdentity, func(), error) {
	var actual bool
	if e := s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM harness_turns WHERE tenant_id=? AND product_id=? AND task_id=?) OR EXISTS(SELECT 1 FROM harness_admin_candidates WHERE tenant_id=? AND product_id=? AND task_id=?)", d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID, d.ProductScope.TenantID, d.ProductScope.ProductID, d.TaskID).Scan(&actual); e != nil {
		return ctx, nil, nil, e
	}
	if !actual {
		return ctx, nil, func() {}, nil
	}
	if s.QueryAuthority == nil {
		return ctx, nil, nil, ErrDenied
	}
	next, id, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	return next, &id, release, e
}
func queryServiceTx(ctx context.Context, tx *sql.Tx, id domain.QueryServiceIdentity) error {
	if id.Installation == "" || id.Generation == 0 || id.ServiceID != "r0-mock-original-query" || id.Action != "attempt.cost.query" || id.Policy != "R0_ORIGINAL_MOCK_COST_V1" {
		return ErrDenied
	}
	var valid bool
	e := tx.QueryRowContext(ctx, "SELECT s.enabled=1 AND i.generation=? FROM harness_query_services s JOIN memory_installation i ON i.installation_id=s.installation_id WHERE s.installation_id=? AND s.service_id=? AND s.action=? AND s.policy=? FOR UPDATE", id.Generation, id.Installation, id.ServiceID, id.Action, id.Policy).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}
func bindOriginalQuery(ctx context.Context, u *unit, d DispatchRequest, service *domain.QueryServiceIdentity) error {
	if service == nil {
		return nil
	}
	if e := queryServiceTx(ctx, u.tx, *service); e != nil {
		return e
	}
	id := mockIdentity(d)
	raw, e := json.Marshal(id)
	if e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO harness_original_queries(tenant_id,product_id,task_id,attempt_id,installation_id,service_id,policy,marker_key,request_identity,generation,next_query_at) VALUES(?,?,?,?,?,?,?,?,?,?,DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 SECOND))", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, service.Installation, service.ServiceID, service.Policy, id.MarkerKey, raw, service.Generation)
	return e
}
func (s *Store) OriginalMockIdentity(ctx context.Context, scope domain.Scope, task string) (domain.MockRequestIdentity, error) {
	var attempt string
	e := s.DB.QueryRowContext(ctx, "SELECT attempt_id FROM attempts WHERE tenant_id=? AND product_id=? AND task_id=? AND slot=0", scope.TenantID, scope.ProductID, task).Scan(&attempt)
	if e != nil {
		return domain.MockRequestIdentity{}, e
	}
	return s.AttemptMockIdentity(ctx, scope, task, attempt)
}

// AttemptMockIdentity selects one exact original provider request. It grants no
// body, dispatch or completion authority, and never falls back to another slot.
func (s *Store) AttemptMockIdentity(ctx context.Context, scope domain.Scope, task, attempt string) (domain.MockRequestIdentity, error) {
	var raw []byte
	var out domain.MockRequestIdentity
	if scope.Validate() != nil || domain.ValidateID(task) != nil || domain.ValidateID(attempt) != nil {
		return out, ErrDenied
	}
	e := s.DB.QueryRowContext(ctx, "SELECT q.request_identity FROM harness_original_queries q JOIN harness_turns h ON h.tenant_id=q.tenant_id AND h.product_id=q.product_id AND h.task_id=q.task_id WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=? AND h.world_instance_id=? AND h.owner_subject_id=? AND q.attempt_id=?", scope.TenantID, scope.ProductID, task, scope.WorldInstanceID, scope.SubjectID, attempt).Scan(&raw)
	if e != nil {
		return out, e
	}
	if json.Unmarshal(raw, &out) != nil || out.Scope != scope || out.TaskID != task || out.AttemptID != attempt {
		return domain.MockRequestIdentity{}, ErrConflict
	}
	return out, nil
}

// DueOriginalQuery is a bounded fixed service scan, not a second task system.
// It never acquires an ordinary narrative lease or changes task state.
func (s *Store) DueOriginalQuery(ctx context.Context) (domain.MockRequestIdentity, error) {
	var id domain.MockRequestIdentity
	if s.QueryAuthority == nil {
		return id, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return id, e
	}
	defer release()
	var raw []byte
	e = s.DB.QueryRowContext(ctx, "SELECT q.request_identity FROM harness_original_queries q JOIN harness_envelopes e USING(tenant_id,product_id,task_id) JOIN harness_query_services s ON s.installation_id=q.installation_id AND s.service_id=q.service_id JOIN memory_installation i ON i.installation_id=s.installation_id WHERE q.installation_id=? AND s.service_id=? AND s.action=? AND s.policy=? AND q.policy=s.policy AND s.enabled=1 AND i.generation=? AND q.provider_state='UNKNOWN' AND q.queries_used<3 AND q.next_query_at<=UTC_TIMESTAMP(6) AND (q.lease_expires_at IS NULL OR q.lease_expires_at<=UTC_TIMESTAMP(6)) AND e.execute_by>UTC_TIMESTAMP(6) ORDER BY q.next_query_at,q.tenant_id,q.product_id,q.task_id,q.attempt_id LIMIT 1", service.Installation, service.ServiceID, service.Action, service.Policy, service.Generation).Scan(&raw)
	if e == sql.ErrNoRows {
		return id, ErrNoTask
	}
	if e != nil {
		return id, e
	}
	e = json.Unmarshal(raw, &id)
	return id, e
}
func (s *Store) BeginOriginalQuery(ctx context.Context, id domain.MockRequestIdentity) (OriginalQueryLease, error) {
	return s.beginOriginalQuery(ctx, id, false)
}
func (s *Store) beginOriginalQuery(ctx context.Context, id domain.MockRequestIdentity, reply bool) (OriginalQueryLease, error) {
	var lease OriginalQueryLease
	if s.QueryAuthority == nil || s.ProviderEvidence == nil || id.Validate() != nil {
		return lease, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return lease, e
	}
	defer release()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return lease, e
	}
	defer tx.Rollback()
	if e = lockOriginalQuery(ctx, tx, id, service); e != nil {
		return lease, e
	}
	var raw, hash []byte
	var installation, serviceID, policy, owner, request, route, markerKey, phase string
	var world sql.NullString
	var fence uint64
	var active, due, inTime bool
	var used int
	e = tx.QueryRowContext(ctx, "SELECT q.request_identity,q.installation_id,q.service_id,q.policy,q.fence,COALESCE(q.lease_expires_at>UTC_TIMESTAMP(6),0),q.next_query_at<=UTC_TIMESTAMP(6),q.queries_used,t.owner_subject_id,t.world_instance_id,a.request_key,a.request_hash,a.route,a.marker_commit_key,c.phase,e.execute_by>UTC_TIMESTAMP(6) FROM harness_original_queries q JOIN tasks t ON t.tenant_id=q.tenant_id AND t.product_id=q.product_id AND t.task_id=q.task_id JOIN attempts a ON a.tenant_id=q.tenant_id AND a.product_id=q.product_id AND a.task_id=q.task_id AND a.attempt_id=q.attempt_id JOIN commit_receipts c ON c.commit_key=q.marker_key AND c.tenant_id=q.tenant_id AND c.product_id=q.product_id AND c.task_id=q.task_id AND c.owner_subject_id=t.owner_subject_id AND c.world_instance_id<=>t.world_instance_id JOIN harness_envelopes e ON e.tenant_id=t.tenant_id AND e.product_id=t.product_id AND e.task_id=t.task_id WHERE q.tenant_id=? AND q.product_id=? AND q.task_id=? AND q.attempt_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&raw, &installation, &serviceID, &policy, &fence, &active, &due, &used, &owner, &world, &request, &hash, &route, &markerKey, &phase, &inTime)
	if e != nil {
		return lease, e
	}
	var original domain.MockRequestIdentity
	if json.Unmarshal(raw, &original) != nil || original != id || installation != service.Installation || serviceID != service.ServiceID || policy != service.Policy || owner != id.Scope.SubjectID || world.String != id.Scope.WorldInstanceID || request != id.RequestKey || hex.EncodeToString(hash) != id.RequestHash || route != id.RouteID || markerKey != id.MarkerKey || phase != "SEND" || active || fence == ^uint64(0) {
		return lease, ErrDenied
	}
	if !reply && (!due || used >= 3 || !inTime) {
		return lease, ErrDenied
	}
	if e = validateOriginalQueryHistory(ctx, tx, id, used); e != nil {
		return lease, e
	}
	kind, ordinal := originalQueryKind(reply), used+1
	if reply {
		ordinal = 0
	}
	var nonce [16]byte
	if _, e = rand.Read(nonce[:]); e != nil {
		return lease, e
	}
	lease = OriginalQueryLease{identity: id, service: service, fence: fence + 1, owner: hex.EncodeToString(nonce[:]), reply: reply, ioPermit: &queryIOPermit{}, queryKey: originalQueryKey(id, kind, ordinal)}
	if reply {
		_, e = tx.ExecContext(ctx, "UPDATE harness_original_queries SET fence=?,lease_owner=?,lease_expires_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 5 SECOND),generation=? WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", lease.fence, lease.owner, service.Generation, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID)
	} else {
		_, e = tx.ExecContext(ctx, "UPDATE harness_original_queries q JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) SET q.fence=?,q.lease_owner=?,q.lease_expires_at=DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 5 SECOND),q.generation=?,q.next_query_at=DATE_ADD(a.send_started_at,INTERVAL IF(q.queries_used=0,5,15) SECOND),q.queries_used=q.queries_used+1 WHERE q.tenant_id=? AND q.product_id=? AND q.task_id=? AND q.attempt_id=?", lease.fence, lease.owner, service.Generation, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID)
	}
	if e != nil {
		return OriginalQueryLease{}, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO harness_original_query_records(tenant_id,product_id,task_id,attempt_id,kind,ordinal,query_key,lease_fence,began_at) VALUES(?,?,?,?,?,?,?,?,UTC_TIMESTAMP(6))", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, kind, ordinal, lease.queryKey, lease.fence); e != nil {
		return OriginalQueryLease{}, e
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return OriginalQueryLease{identity: id, queryKey: lease.queryKey}, e
	}
	return lease, nil
}
func (s *Store) RecordOriginalQuery(ctx context.Context, lease OriginalQueryLease) (out domain.MockResultMetadata, err error) {
	defer func() {
		if err != nil {
			out = domain.MockResultMetadata{}
		}
	}()
	if lease.cost || lease.fence == 0 || lease.owner == "" || s.QueryAuthority == nil || s.ProviderEvidence == nil {
		return out, ErrDenied
	}
	ctx, service, release, e := s.QueryAuthority.EnterOriginalQuery(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	if service != lease.service {
		return out, ErrDenied
	}
	check, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return out, e
	}
	if e = lockOriginalQuery(ctx, check, lease.identity, service); e == nil {
		e = validateQueryLease(ctx, check, lease)
	}
	if e == nil {
		e = validateOriginalQueryReceipt(ctx, check, lease)
	}
	if rollbackError := check.Rollback(); e == nil && rollbackError != nil {
		e = rollbackError
	}
	if e != nil {
		return out, e
	}
	if lease.ioPermit == nil || !lease.ioPermit.taken.CompareAndSwap(false, true) {
		return out, ErrDenied
	}
	// Independent journal I/O finishes before acquiring any business row lock.
	queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, e = s.ProviderEvidence.OriginalMetadata(queryCtx, lease.identity)
	if e != nil {
		return out, e
	}
	if out.Identity != lease.identity || (out.State != "UNKNOWN" && out.State != "SUCCEEDED") {
		return domain.MockResultMetadata{}, ErrDenied
	}
	if out.State == "SUCCEEDED" && !validOriginalUsageMetadata(minimalOriginalMetadata(out)) {
		return out, ErrDenied
	}
	if lease.reply && out.State != "SUCCEEDED" {
		return out, ErrDenied
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	id := lease.identity
	if e = lockOriginalQuery(ctx, tx, id, service); e != nil {
		return out, e
	}
	var fence uint64
	var owner string
	var generation uint64
	var valid bool
	e = tx.QueryRowContext(ctx, "SELECT fence,lease_owner,generation,lease_expires_at>UTC_TIMESTAMP(6) FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? FOR UPDATE", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&fence, &owner, &generation, &valid)
	if e != nil {
		return out, e
	}
	if fence != lease.fence || owner != lease.owner || generation != service.Generation || !valid {
		return out, ErrDenied
	}
	if e = validateOriginalQueryReceipt(ctx, tx, lease); e != nil {
		return out, e
	}
	raw, e := json.Marshal(out)
	if e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_original_queries SET metadata=?,provider_state=?,lease_expires_at=NULL,lease_owner=NULL WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", raw, out.State, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID); e != nil {
		return out, e
	}
	if out.State == "SUCCEEDED" {
		if _, e = tx.ExecContext(ctx, "UPDATE attempts SET state='SUCCEEDED' WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND request_key=? AND request_hash=? AND marker_commit_key=? AND state IN ('SENT','UNKNOWN','SUCCEEDED')", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, id.RequestKey, mustHash(id.RequestHash), id.MarkerKey); e != nil {
			return out, e
		}
	}
	minimal, e := json.Marshal(minimalOriginalMetadata(out))
	if e != nil {
		return out, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE harness_original_query_records SET metadata=?,recorded_at=UTC_TIMESTAMP(6) WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=? AND query_key=? AND lease_fence=? AND metadata IS NULL", minimal, id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID, lease.queryKey, lease.fence); e != nil {
		return out, e
	}
	if e = s.costCommit(ctx, tx); e != nil {
		return out, e
	}
	return out, nil
}
func mustHash(value string) []byte { b, _ := hex.DecodeString(value); return b }

func validateQueryLease(ctx context.Context, tx *sql.Tx, lease OriginalQueryLease) error {
	id := lease.identity
	var fence, generation uint64
	var owner sql.NullString
	var valid bool
	if e := tx.QueryRowContext(ctx, "SELECT fence,lease_owner,generation,COALESCE(lease_expires_at>UTC_TIMESTAMP(6),0) FROM harness_original_queries WHERE tenant_id=? AND product_id=? AND task_id=? AND attempt_id=?", id.Scope.TenantID, id.Scope.ProductID, id.TaskID, id.AttemptID).Scan(&fence, &owner, &generation, &valid); e != nil {
		return e
	}
	if !valid || owner.String != lease.owner || fence != lease.fence || generation != lease.service.Generation {
		return ErrDenied
	}
	return nil
}
func (s *Store) ObserveOriginalReply(ctx context.Context, permit *OriginalSendPermit) (domain.MockResultMetadata, error) {
	if permit == nil || permit.store != s || !permit.taken.Load() || !permit.replied.CompareAndSwap(false, true) {
		return domain.MockResultMetadata{}, ErrDenied
	}
	id := mockIdentity(permit.dispatch)
	if s.ProviderEvidence == nil {
		return domain.MockResultMetadata{}, ErrDenied
	}
	lease, e := s.beginOriginalQuery(ctx, id, true)
	if e != nil {
		return domain.MockResultMetadata{}, e
	}
	return s.RecordOriginalQuery(ctx, lease)
}
