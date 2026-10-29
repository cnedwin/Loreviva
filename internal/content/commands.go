package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wire"
	"sync"
)

var ErrNotFound = errors.New("content missing or not visible")
var ErrBlocked = errors.New("content check blocks requested operation")

type Conflict struct{ DraftID, Expected, Current, Hash string }

func (c *Conflict) Error() string { return "draft revision conflict; preserve local edits" }

type FieldErrors struct{ Findings []generated.FieldError }

func (e *FieldErrors) Error() string { return "definition field validation failed" }

type Service struct {
	Store       *store.Store
	cursorOnce  sync.Once
	cursorKey   [32]byte
	cursorError error
}

type originalPayloadKey struct{}

func WithOriginalPayload(ctx context.Context, raw []byte) context.Context {
	return context.WithValue(ctx, originalPayloadKey{}, json.RawMessage(append([]byte(nil), raw...)))
}

func New(s *store.Store) *Service {
	service := &Service{Store: s}
	s.PackagePreviewOwner = service
	return service
}
func Scope(s domain.ProductScope) generated.ProductScope {
	return generated.ProductScope{TenantID: s.TenantID, ProductID: s.ProductID}
}
func Identity(parts ...string) string {
	b, _ := json.Marshal(parts)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func Key(s domain.ProductScope, kind, id string) store.LockKey {
	return store.LockKey{Rank: 90, Kind: kind, IDs: []string{s.TenantID, s.ProductID, id}}
}
func Gate(ctx context.Context, s *store.Store, scope domain.ProductScope) (store.GateVersions, error) {
	var g store.GateVersions
	if e := scope.Validate(); e != nil {
		return g, ErrNotFound
	}
	var ok bool
	e := s.DB.QueryRowContext(ctx, "SELECT t.permission_version,p.permission_version,s.permission_version,s.privacy_epoch,s.view_epoch,(t.enabled AND p.enabled AND s.enabled AND s.can_execute) FROM subject_governance s JOIN product_gates p ON p.tenant_id=s.tenant_id AND p.product_id=s.product_id JOIN tenant_gates t ON t.tenant_id=s.tenant_id WHERE s.tenant_id=? AND s.product_id=? AND s.subject_id=?", scope.TenantID, scope.ProductID, scope.SubjectID).Scan(&g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &ok)
	if e != nil || !ok {
		return g, ErrNotFound
	}
	return g, nil
}

// Command stores the original typed result in the same UoW as its receipt. All
// current gate checks still run on replay; revision/expiry checks run only in body.
func Command(ctx context.Context, s *store.Store, scope domain.ProductScope, operation, key, target, schema string, payload any, locks []store.LockKey, body func(store.Tx) (any, error)) (json.RawMessage, error) {
	return BoundCommand(ctx, s, scope, operation, key, target, schema, payload, locks, store.CommandBarrierRequest{}, body)
}

// BoundCommand shares the exact original-payload normalizer and durable result
// protocol with content commands; the Store validates closed runtime bindings.
func BoundCommand(ctx context.Context, s *store.Store, scope domain.ProductScope, operation, key, target, schema string, payload any, locks []store.LockKey, binding store.CommandBarrierRequest, body func(store.Tx) (any, error)) (json.RawMessage, error) {
	if operation == "commandContentDraft" {
		b, e := json.Marshal(payload)
		if e != nil {
			return nil, e
		}
		var v struct{ Action string }
		if e = json.Unmarshal(b, &v); e != nil {
			return nil, e
		}
		binding.ContentAction = v.Action
	}
	if original, ok := ctx.Value(originalPayloadKey{}).(json.RawMessage); ok {
		payload = original
	}
	if e := domain.ValidateIdempotencyKey(key); e != nil {
		return nil, e
	}
	g, e := Gate(ctx, s, scope)
	if e != nil {
		return nil, e
	}
	raw, e := json.Marshal(struct {
		Scope             domain.ProductScope
		Operation, Target string
		Input             any
	}{scope, operation, target, payload})
	if e != nil {
		return nil, e
	}
	canonical, e := wire.Canonical(raw)
	if e != nil {
		return nil, e
	}
	hash := sha256.Sum256(canonical)
	resultID := Identity(scope.TenantID, scope.ProductID, scope.SubjectID, operation, key)
	locks = append(locks, Key(scope, "content_check", resultID))
	binding.ProductScope = scope
	binding.OwnerSubjectID = scope.SubjectID
	binding.Operation = operation
	binding.CommandKey = key
	binding.CommitKey = resultID
	binding.GateVersions = g
	binding.PayloadHash = hash
	binding.Locks = locks
	_, e = s.WithCommandBarrier(ctx, binding, func(tx store.Tx) error {
		value, e := body(tx)
		if e != nil {
			return e
		}
		raw, e := json.Marshal(value)
		if e != nil {
			return e
		}
		if e = generated.Validate(schema, raw); e != nil {
			return e
		}
		if e = tx.RequireLock(Key(scope, "content_check", resultID)); e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO admin_results(tenant_id,product_id,result_id,owner_subject_id,schema_name,document) VALUES(?,?,?,?,?,?)", scope.TenantID, scope.ProductID, resultID, scope.SubjectID, schema, raw)
		return e
	})
	if e != nil {
		return nil, e
	}
	var document []byte
	e = s.DB.QueryRowContext(ctx, "SELECT document FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? AND owner_subject_id=? AND schema_name=?", scope.TenantID, scope.ProductID, resultID, scope.SubjectID, schema).Scan(&document)
	if e != nil {
		return nil, e
	}
	return document, nil
}
