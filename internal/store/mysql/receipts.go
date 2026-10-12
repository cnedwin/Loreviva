package mysql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"loreviva.local/loreviva/internal/domain"
)

type receiptRow struct {
	key, tenant, product, owner, kind, task, operation, command, phase string
	world                                                              sql.NullString
	hash                                                               []byte
	nullTask                                                           sql.NullString
}
type rowQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func queryReceipt(ctx context.Context, q rowQuerier, key string) (r receiptRow, err error) {
	err = q.QueryRowContext(ctx, "SELECT commit_key,tenant_id,product_id,owner_subject_id,scope_kind,world_instance_id,task_id,operation,command_key,payload_hash,phase FROM commit_receipts WHERE commit_key=?", key).Scan(&r.key, &r.tenant, &r.product, &r.owner, &r.kind, &r.world, &r.nullTask, &r.operation, &r.command, &r.hash, &r.phase)
	r.task = r.nullTask.String
	return
}
func (r receiptRow) public() CommitReceipt {
	return CommitReceipt{Key: r.key, TaskID: r.task, OwnerSubjectID: r.owner, Committed: true}
}
func (r receiptRow) matches(b BarrierRequest) bool {
	k, _, _ := scopeOf(b)
	return r.tenant == b.ProductScope.TenantID && r.product == b.ProductScope.ProductID && r.owner == b.OwnerSubjectID && r.kind == k && r.world.Valid == (b.WorldScope != nil) && r.world.String == worldKey(b) && r.task == b.TaskID && r.operation == b.Operation && r.command == b.CommandKey && r.phase == receiptPhase(b) && bytes.Equal(r.hash, b.PayloadHash[:])
}

// QueryCommit is repository-internal; stable IDs are never permissions.
func (s *Store) QueryCommit(ctx context.Context, key string) (CommitReceipt, error) {
	if e := domain.ValidateIdempotencyKey(key); e != nil {
		return CommitReceipt{}, e
	}
	r, e := queryReceipt(ctx, s.DB, key)
	if e != nil {
		return CommitReceipt{}, e
	}
	return r.public(), nil
}
func (s *Store) QueryCommitScoped(ctx context.Context, scope domain.ProductScope, world *domain.Scope, owner, key string) (CommitReceipt, error) {
	return queryCommitScopedRow(ctx, s.DB, scope, world, owner, key)
}
func queryCommitScopedRow(ctx context.Context, q rowQuerier, scope domain.ProductScope, world *domain.Scope, owner, key string) (CommitReceipt, error) {
	if e := (domain.TaskScope{ProductScope: scope, WorldScope: world, OwnerSubjectID: owner}).Validate(); e != nil {
		return CommitReceipt{}, ErrDenied
	}
	if e := domain.ValidateIdempotencyKey(key); e != nil {
		return CommitReceipt{}, e
	}
	// All owner/current governance predicates and receipt scope are one primary read.
	r := BarrierRequest{ProductScope: scope, WorldScope: world, OwnerSubjectID: owner}
	kind, w, _ := scopeOf(r)
	var result CommitReceipt
	var task sql.NullString
	e := q.QueryRowContext(ctx, "SELECT c.commit_key,c.task_id,c.owner_subject_id FROM commit_receipts c JOIN tenant_gates t ON t.tenant_id=c.tenant_id JOIN product_gates p ON p.tenant_id=c.tenant_id AND p.product_id=c.product_id JOIN subject_governance g ON g.tenant_id=c.tenant_id AND g.product_id=c.product_id AND g.subject_id=c.owner_subject_id WHERE c.commit_key=? AND c.tenant_id=? AND c.product_id=? AND c.owner_subject_id=? AND c.scope_kind=? AND c.world_instance_id<=>? AND t.enabled=1 AND p.enabled=1 AND g.enabled=1", key, scope.TenantID, scope.ProductID, owner, kind, w).Scan(&result.Key, &task, &result.OwnerSubjectID)
	if errors.Is(e, sql.ErrNoRows) {
		return CommitReceipt{}, ErrDenied
	}
	if e != nil {
		return CommitReceipt{}, e
	}
	result.Committed = true
	result.TaskID = task.String
	return result, nil
}
