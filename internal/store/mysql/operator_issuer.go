package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"loreviva.local/loreviva/internal/domain"
)

// EnterDraftTaskIssuer reconstructs only an existing root's authenticated issuer
// from its exact registered family, intake receipt and currently held lease.
// It cannot produce a direct-user context or authorize new user commands.
func (s *Store) EnterDraftTaskIssuer(ctx context.Context, lease domain.TaskLease, worker string) (context.Context, error) {
	if lease.WorldInstanceID != nil || lease.SubjectID != lease.OwnerSubjectID || worker == "" || lease.Fence == 0 {
		return ctx, ErrDenied
	}
	var credential, family, provenance string
	var hash []byte
	e := s.DB.QueryRowContext(ctx, `SELECT j.issuer_credential_id,j.operation,j.issuer_provenance,c.token_sha256 FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) JOIN operator_credentials c ON c.credential_id=j.issuer_credential_id AND c.tenant_id=t.tenant_id AND c.product_id=t.product_id AND c.subject_id=t.owner_subject_id WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND t.task_kind='ADMIN' AND t.world_instance_id IS NULL`, lease.TenantID, lease.ProductID, lease.TaskID, lease.SubjectID).Scan(&credential, &family, &provenance, &hash)
	if e != nil || len(hash) != 32 || (family != "CANDIDATE" && family != "REHEARSAL") || (provenance != "DIRECT_OPERATOR" && provenance != "LEGACY_ORIGINAL_OWNER") || provenance == "LEGACY_ORIGINAL_OWNER" && credential != "owner" {
		return ctx, ErrDenied
	}
	p := currentOperator{store: s, principal: domain.OperatorPrincipal{CredentialID: credential, ProductScope: lease.ProductScope}, job: lease.TaskID, family: family, worker: worker, fence: lease.Fence, kind: "ADMIN"}
	copy(p.bearerHash[:], hash)
	next := context.WithValue(ctx, operatorContextKey{}, p)
	// Reconstructed provenance does not itself demand a new-purchase grant. The
	// particular owning path rechecks its current action before any new work.
	if e = checkDraftIssuerProof(next, s.DB, p); e != nil {
		return ctx, e
	}
	return next, nil
}

// For reconstructed jobs, require the current lease, fence, worker, registered issuer and exact intake together.
// An empty job is outside this reconstruction proof; this helper alone grants no action.
func checkDraftIssuerProof(ctx context.Context, q rowQuerier, p currentOperator) error {
	if p.job == "" {
		return nil
	}
	var valid bool
	e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND t.task_kind=? AND COALESCE(t.world_instance_id,'')=? AND t.fence=? AND t.lease_owner=? AND t.lease_expires_at>UTC_TIMESTAMP(6) AND t.state IN ('RUNNING','COMMITTED') AND j.operation=? AND j.issuer_credential_id=? AND (j.issuer_provenance='DIRECT_OPERATOR' OR (j.issuer_provenance='LEGACY_ORIGINAL_OWNER' AND j.operation IN ('CANDIDATE','REHEARSAL') AND j.issuer_credential_id='owner')))`, p.principal.TenantID, p.principal.ProductID, p.job, p.principal.SubjectID, p.kind, p.world, p.fence, p.worker, p.family, p.principal.CredentialID).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return ErrDenied
	}
	if p.family == "INSTANCE_CONTROL" || p.family == "INSTANCE_MIGRATION" {
		rows, ok := q.(interface {
			rowQuerier
			QueryContext(context.Context, string, ...any) (*sql.Rows, error)
		})
		if !ok {
			return ErrDenied
		}
		return inspectLegacyAdminIntake(ctx, rows, p.principal.ProductScope, AdminTaskMetadata{ID: p.job, Kind: p.family, World: p.world})
	}
	valid, e = studioExactDraftIntake(ctx, q, p.principal.ProductScope, p.job, p.family)
	if e != nil && e != sql.ErrNoRows {
		return e
	}
	if !valid {
		return ErrDenied
	}
	return nil
}

func (s *Store) EnterControlTaskIssuer(ctx context.Context, lease RegisteredLease, worker string) (context.Context, error) {
	var provenance string
	if e := s.DB.QueryRowContext(ctx, "SELECT issuer_provenance FROM registered_jobs WHERE tenant_id=? AND product_id=? AND task_id=?", lease.TenantID, lease.ProductID, lease.TaskID).Scan(&provenance); e != nil {
		return ctx, e
	}
	if provenance == "DIRECT_INTEGRATION" {
		return s.enterIntegrationControlIssuer(ctx, lease, worker)
	}
	if lease.WorldInstanceID == nil || lease.SubjectID != lease.OwnerSubjectID || worker == "" || lease.Fence == 0 || !(lease.Kind == "CONTROL" && lease.Operation == "INSTANCE_CONTROL" || lease.Kind == "MIGRATION" && lease.Operation == "INSTANCE_MIGRATION") {
		return ctx, ErrDenied
	}
	var credential string
	var hash []byte
	e := s.DB.QueryRowContext(ctx, `SELECT j.issuer_credential_id,c.token_sha256 FROM registered_jobs j JOIN operator_credentials c ON c.credential_id=j.issuer_credential_id AND c.tenant_id=j.tenant_id AND c.product_id=j.product_id AND c.subject_id=? WHERE j.tenant_id=? AND j.product_id=? AND j.task_id=? AND j.operation=? AND j.issuer_provenance='DIRECT_OPERATOR'`, lease.SubjectID, lease.TenantID, lease.ProductID, lease.TaskID, lease.Operation).Scan(&credential, &hash)
	if e != nil || len(hash) != 32 {
		return ctx, ErrDenied
	}
	p := currentOperator{store: s, principal: domain.OperatorPrincipal{CredentialID: credential, ProductScope: lease.ProductScope}, job: lease.TaskID, family: lease.Operation, worker: worker, world: *lease.WorldInstanceID, kind: lease.Kind, fence: lease.Fence}
	copy(p.bearerHash[:], hash)
	if e = checkDraftIssuerProof(ctx, s.DB, p); e != nil {
		return ctx, e
	}
	return context.WithValue(ctx, operatorContextKey{}, p), nil
}

// CheckProgramCleanup proves a particular already accepted obligation; it mints
// no general program principal and deliberately does not consult user expiry.
func (s *Store) CheckProgramCleanup(ctx context.Context, lease RegisteredLease, worker string) error {
	return checkProgramCleanup(ctx, s.DB, lease, worker, false)
}

type programQuery interface {
	rowQuerier
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func checkProgramCleanup(ctx context.Context, q programQuery, lease RegisteredLease, worker string, committing bool) error {
	if !(lease.Kind == "ADMIN" && lease.Operation == "FILE_CLEANUP" && lease.WorldInstanceID == nil || lease.Kind == "GOVERNANCE" && lease.Operation == "MEMORY_GOVERNANCE" && lease.WorldInstanceID != nil) {
		return ErrDenied
	}
	var world string
	if lease.WorldInstanceID != nil {
		world = *lease.WorldInstanceID
	}
	var valid bool
	e := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks t JOIN registered_jobs j USING(tenant_id,product_id,task_id) WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=? AND t.owner_subject_id=? AND t.task_kind=? AND COALESCE(t.world_instance_id,'')=? AND t.fence=? AND t.lease_owner=? AND t.lease_expires_at>UTC_TIMESTAMP(6) AND (t.state='RUNNING' OR (? AND (t.state='COMMITTED' OR (j.operation='FILE_CLEANUP' AND t.state='ACCEPTED')))) AND j.operation=? AND j.issuer_provenance='PROGRAM_OWNED' AND j.issuer_credential_id IS NULL)`, lease.TenantID, lease.ProductID, lease.TaskID, lease.SubjectID, lease.Kind, world, lease.Fence, worker, committing, lease.Operation).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid || lease.SubjectID != lease.OwnerSubjectID {
		return ErrDenied
	}
	if lease.Operation == "FILE_CLEANUP" {
		return inspectLegacyAdminIntake(ctx, q, lease.ProductScope, AdminTaskMetadata{ID: lease.TaskID, Kind: lease.Operation})
	}
	return programPrivacyIntent(ctx, q, lease.ProductScope, lease.TaskID, world)
}

// Commit checking also accepts a FILE_CLEANUP page's fenced ACCEPTED continuation.
// The public entry remains RUNNING-only; barrier rank50 and the pre-body check
// require RUNNING before any new execution and reject old fences.
func checkProgramBarrier(ctx context.Context, tx *sql.Tx, r BarrierRequest) error {
	if r.commandOnly || (r.Operation != "FILE_CLEANUP" && r.Operation != "MEMORY_GOVERNANCE") {
		return nil
	}
	l := RegisteredLease{TaskLease: TaskLease{ProductScope: r.ProductScope, OwnerSubjectID: r.OwnerSubjectID, TaskID: r.TaskID, Fence: r.Fence}, Kind: "ADMIN", Operation: r.Operation}
	if r.WorldScope != nil {
		l.WorldInstanceID = &r.WorldScope.WorldInstanceID
		l.Kind = "GOVERNANCE"
	}
	return checkProgramCleanup(ctx, tx, l, r.WorkerID, true)
}

func programPrivacyIntent(ctx context.Context, q rowQuerier, s domain.ProductScope, task, world string) error {
	var hash []byte
	var owner, persistedWorld string
	if e := q.QueryRowContext(ctx, "SELECT world_instance_id,owner_subject_id,request_hash FROM memory_governance WHERE tenant_id=? AND product_id=? AND task_id=?", s.TenantID, s.ProductID, task).Scan(&persistedWorld, &owner, &hash); e != nil {
		return e
	}
	if owner != s.SubjectID || world != persistedWorld || len(hash) != 32 {
		return ErrDenied
	}
	r, e := queryReceipt(ctx, q, turnIdentity(task, "accept"))
	if e != nil {
		return e
	}
	if r.tenant != s.TenantID || r.product != s.ProductID || r.owner != owner || r.kind != "WORLD" || r.world.String != world || r.phase != "COMMIT" || r.operation != "MEMORY_GOVERNANCE_ACCEPT" || !bytes.Equal(r.hash, hash) || task != turnIdentity(r.tenant, r.product, owner, world, "MEMORY_GOVERNANCE", r.command) {
		return ErrDenied
	}
	return nil
}
