package mysql

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/wire"
	"reflect"
)

type DeliveryAdmission interface {
	EnterDelivery(context.Context, *Store) (context.Context, func(), error)
	CheckDelivery(context.Context, *Store) error
}
type DeliveryReaderOwner interface{ DeliveryReaderStore() *Store }

func (s *Store) BindDeliveryReader(owner DeliveryReaderOwner, admission DeliveryAdmission, snapshot DeliverySnapshotOwner) error {
	if owner == nil || reflect.ValueOf(owner).Kind() != reflect.Pointer || reflect.ValueOf(owner).IsNil() || owner.DeliveryReaderStore() != s || s.CheckDeliverySnapshotOwner(snapshot) != nil {
		return ErrDenied
	}
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if s.integrationDeliveryReader != nil || s.integrationDeliveryAdmission != admission {
		return ErrDenied
	}
	guard, ok := admission.(OperationGuard)
	if !ok || guard != s.Guard {
		return ErrDenied
	}
	s.integrationDeliveryReader = owner
	return nil
}
func (s *Store) CheckDeliveryReader(ctx context.Context, owner DeliveryReaderOwner, snapshot DeliverySnapshotOwner) error {
	s.integrationOwnerMu.Lock()
	same := s.integrationDeliveryReader == owner
	s.integrationOwnerMu.Unlock()
	if !same || owner == nil || owner.DeliveryReaderStore() != s || s.CheckDeliverySnapshotOwner(snapshot) != nil {
		return ErrDenied
	}
	return s.checkDeliveryAdmission(ctx)
}

// Bind once to the actual installed Guard object; a same-type replacement is
// not a capability. The real participant verifies installation and live handle.
func (s *Store) BindDeliveryAdmission(d DeliveryAdmission) error {
	if d == nil || reflect.ValueOf(d).Kind() != reflect.Pointer || reflect.ValueOf(d).IsNil() {
		return ErrDenied
	}
	g, ok := d.(OperationGuard)
	if !ok || g != s.Guard {
		return ErrDenied
	}
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if s.integrationDeliveryAdmission != nil {
		return ErrDenied
	}
	s.integrationDeliveryAdmission = d
	return nil
}
func (s *Store) checkDeliveryAdmission(ctx context.Context) error {
	s.integrationOwnerMu.Lock()
	d := s.integrationDeliveryAdmission
	s.integrationOwnerMu.Unlock()
	if d == nil {
		return ErrDenied
	}
	g, ok := d.(OperationGuard)
	if !ok || g != s.Guard {
		return ErrDenied
	}
	return d.CheckDelivery(ctx, s)
}

// DeliveryCommit is data. Only a plan minted from the original executing issuer
// and bound to this exact transaction can append it.
type DeliveryCommit struct {
	Scope                                        domain.Scope
	Kind, TaskID, ResultID, PacketID, ReceiptKey string
	EventIDs, Lineage                            []string
	Revision                                     uint64
	Turn                                         *domain.CandidateCommit
}
type integrationDeliveryPlan struct {
	store                                                                           *Store
	unit                                                                            *unit
	scope                                                                           domain.Scope
	kind, task, result, receipt, customer, player, consumer, update, notice, outbox string
	written                                                                         bool
	expected                                                                        *deliveryExpected
}
type deliveryExpected struct {
	Input                              DeliveryCommit
	H                                  uint64
	Events, Audience, Lineage, Payload []byte
	PayloadHash                        [32]byte
}

func deliveryJSON(v any) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, e
	}
	return wire.Canonical(b)
}
func deliveryKey(p *integrationDeliveryPlan, kind, id string) LockKey {
	return LockKey{Rank: 110, Kind: kind, IDs: []string{p.scope.TenantID, p.scope.ProductID, id}}
}

// Shared runtime results do not imply integration delivery. In particular,
// ACTION_BUNDLE inside TURN and rehearsal/migration must never append twice.
func (s *Store) planIntegrationDelivery(ctx context.Context, r *BarrierRequest) error {
	p := &integrationDeliveryPlan{store: s, task: r.TaskID, receipt: r.CommitKey}
	if r.Operation == "INSTANCE_CONTROL" && r.integrationControl != nil && r.integrationControl.lease != nil {
		a := r.integrationControl
		if a.store != s || r.commandOnly || r.WorldScope == nil || a.document.Task != r.TaskID {
			return ErrDenied
		}
		p.kind = "CONTROL"
		p.scope = *r.WorldScope
		p.customer = a.service.principal.Customer
		p.player = a.eligibility.entitlement.Player
	} else if r.Operation == "HARNESS_TURN_COMMIT" && r.integrationTurn != nil && r.integrationTurn.lease != nil && r.rehearsalExecution == nil {
		a := r.integrationTurn
		if a.store != s || r.commandOnly || r.WorldScope == nil || a.task != r.TaskID {
			return ErrDenied
		}
		p.kind = "TURN"
		p.scope = *r.WorldScope
		p.customer = a.service.principal.Customer
		p.player = a.scope.SubjectID
	} else {
		return nil
	}
	if e := s.checkDeliveryAdmission(ctx); e != nil {
		return e
	}
	if p.scope.Validate() != nil || domain.ValidateID(p.customer) != nil || domain.ValidateID(p.player) != nil {
		return ErrDenied
	}
	p.result = turnIdentity(p.task, "result")
	p.consumer = turnIdentity(p.scope.TenantID, p.scope.ProductID, p.customer, "integration-consumer-v1")
	p.update = turnIdentity(p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, p.kind, p.result, "1")
	p.notice = turnIdentity(p.consumer, p.scope.WorldInstanceID, p.result, "1")
	p.outbox = turnIdentity(p.notice, "integration-outbox")
	r.integrationDelivery = p
	r.Locks = append(r.Locks, deliveryKey(p, "delivery_update", p.update), deliveryKey(p, "integration_notice", p.notice), deliveryKey(p, "outbox", p.outbox))
	return nil
}
func (u *unit) AppendIntegrationControlResult(ctx context.Context, s domain.Scope, task, result string) error {
	if u.integrationControl == nil || u.integrationControl.lease == nil || u.operation != "INSTANCE_CONTROL" {
		return nil
	}
	if u.integrationDelivery == nil {
		return ErrDenied
	}
	return u.operatorStore.AppendIntegrationResultTx(ctx, u, DeliveryCommit{Scope: s, Kind: "CONTROL", TaskID: task, ResultID: result, ReceiptKey: u.integrationDelivery.receipt, EventIDs: []string{result}, Lineage: []string{u.integrationControl.document.Scope.WorldInstanceID}, Revision: 1})
}
func (s *Store) AppendIntegrationResultTx(ctx context.Context, tx Tx, input DeliveryCommit) error {
	u, ok := tx.(*unit)
	if !ok || u == nil || u.operatorStore != s || u.tx == nil || u.integrationDelivery == nil {
		return ErrDenied
	}
	p := u.integrationDelivery
	if p.store != s || p.unit != u || p.written || u.commandOnly || u.taskID != p.task || u.world != p.scope.WorldInstanceID || input.Scope != p.scope || input.Kind != p.kind || input.TaskID != p.task || input.ResultID != p.result || input.ReceiptKey != p.receipt || input.Revision != 1 {
		return ErrDenied
	}
	if p.kind == "CONTROL" {
		if input.PacketID != "" || input.Turn != nil || !reflect.DeepEqual(input.EventIDs, []string{p.result}) {
			return ErrDenied
		}
	} else {
		if input.Turn == nil || input.Turn.ResultID != p.result || input.Turn.ReceiptKey != p.receipt || input.PacketID != p.result || !reflect.DeepEqual(input.EventIDs, input.Turn.EventIDs) {
			return ErrDenied
		}
	}
	if e := u.RequireLock(LockKey{Rank: 40, IDs: []string{p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID}}); e != nil {
		return e
	}
	var h uint64
	if e := u.tx.QueryRowContext(ctx, "SELECT update_seq FROM delivery_streams WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID).Scan(&h); e != nil {
		return e
	}
	if h == ^uint64(0) {
		return ErrConflict
	}
	h++
	for _, k := range []LockKey{deliveryKey(p, "delivery_update", p.update), deliveryKey(p, "integration_notice", p.notice), deliveryKey(p, "outbox", p.outbox)} {
		if e := u.RequireLock(k); e != nil {
			return e
		}
	}
	// Clone actual write arguments before any SQL/fault hook can alter records.
	raw, e := json.Marshal(input)
	if e != nil {
		return e
	}
	var copy DeliveryCommit
	if json.Unmarshal(raw, &copy) != nil {
		return ErrDenied
	}
	x := &deliveryExpected{Input: copy, H: h}
	x.Events, e = deliveryJSON(input.EventIDs)
	if e != nil {
		return e
	}
	x.Audience, e = deliveryJSON([]string{p.player})
	if e != nil {
		return e
	}
	x.Lineage, e = deliveryJSON(input.Lineage)
	if e != nil {
		return e
	}
	kind, summary := "CONTROL_COMMITTED", "SYNTHETIC_CONTROL_COMMITTED"
	if p.kind == "TURN" {
		kind, summary = "RESULT_COMMITTED", "SYNTHETIC_RESULT_COMMITTED"
	}
	x.Payload, e = deliveryJSON(struct {
		Kind, Summary, Notice, Result, Message string
		Revision                               uint64
	}{kind, summary, p.notice, p.result, p.notice, 1})
	if e != nil {
		return e
	}
	x.PayloadHash = sha256.Sum256(x.Payload)
	res, e := u.tx.ExecContext(ctx, "UPDATE delivery_streams SET update_seq=? WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND update_seq=?", h, p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, h-1)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return ErrConflict
	}
	var packet any
	if input.PacketID != "" {
		packet = input.PacketID
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO delivery_updates VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)", p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, h, p.update, p.kind, p.result, p.task, string(x.Events), packet, "COMMITTED", 1, string(x.Audience), string(x.Lineage))
	if e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO integration_notifications VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)", p.scope.TenantID, p.scope.ProductID, p.customer, p.notice, p.consumer, p.scope.WorldInstanceID, p.result, 1, kind, x.PayloadHash[:], p.notice, p.outbox, string(x.Lineage))
	if e != nil {
		return e
	}
	_, e = u.tx.ExecContext(ctx, "INSERT INTO outbox(tenant_id,product_id,record_id,task_id,owner_subject_id,consumer_id,message_id,status,revision) VALUES(?,?,?,?,?,?,?,'PENDING',1)", p.scope.TenantID, p.scope.ProductID, p.outbox, p.task, p.scope.SubjectID, p.consumer, p.notice)
	if e != nil {
		return e
	}
	p.written = true
	p.expected = x
	return nil
}

func (s *Store) finishIntegrationDelivery(ctx context.Context, u *unit, r BarrierRequest) error {
	p := r.integrationDelivery
	if p == nil {
		return nil
	}
	if e := s.checkDeliveryAdmission(ctx); e != nil {
		return e
	}
	if u == nil || u.integrationDelivery != p || p.store != s || p.unit != u || !p.written || p.expected == nil || p.task != r.TaskID || p.receipt != r.CommitKey {
		return ErrDenied
	}
	receipt, e := queryReceipt(ctx, u.tx, r.CommitKey)
	if e != nil || !receipt.matches(r) {
		return ErrDenied
	}
	x := p.expected
	var h, revision uint64
	var kind, result, task, state string
	var packet sql.NullString
	var events, audience, lineage []byte
	e = u.tx.QueryRowContext(ctx, "SELECT d.update_seq,u.kind,u.result_ref,u.task_ref,u.event_refs,u.packet_ref,u.delivery_state,u.resource_revision,u.audience_refs,u.lineage_refs FROM delivery_streams d JOIN delivery_updates u USING(tenant_id,product_id,world_instance_id) WHERE d.tenant_id=? AND d.product_id=? AND d.world_instance_id=? AND u.update_seq=? AND u.business_update_id=?", p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, x.H, p.update).Scan(&h, &kind, &result, &task, &events, &packet, &state, &revision, &audience, &lineage)
	canonicalEqual := func(a, b []byte) bool { v, e := wire.Canonical(a); return e == nil && string(v) == string(b) }
	if e != nil || h != x.H || kind != p.kind || result != p.result || task != p.task || state != "COMMITTED" || revision != 1 || packet.Valid != (x.Input.PacketID != "") || packet.String != x.Input.PacketID || !canonicalEqual(events, x.Events) || !canonicalEqual(audience, x.Audience) || !canonicalEqual(lineage, x.Lineage) {
		return ErrConflict
	}
	var good bool
	noticeKind := "CONTROL_COMMITTED"
	if p.kind == "TURN" {
		noticeKind = "RESULT_COMMITTED"
	}
	e = u.tx.QueryRowContext(ctx, "SELECT (n.consumer_id=? AND n.world_instance_id=? AND n.result_ref=? AND n.initial_revision=1 AND n.kind=? AND n.payload_hash=? AND n.message_ref=? AND n.outbox_ref=? AND o.task_id=? AND o.owner_subject_id=? AND o.consumer_id=? AND o.message_id=? AND o.status='PENDING' AND o.revision=1),n.visibility_source_refs FROM integration_notifications n JOIN outbox o ON o.tenant_id=n.tenant_id AND o.product_id=n.product_id AND o.record_id=n.outbox_ref WHERE n.tenant_id=? AND n.product_id=? AND n.customer_id=? AND n.notice_ref=?", p.consumer, p.scope.WorldInstanceID, p.result, noticeKind, x.PayloadHash[:], p.notice, p.outbox, p.task, p.scope.SubjectID, p.consumer, p.notice, p.scope.TenantID, p.scope.ProductID, p.customer, p.notice).Scan(&good, &lineage)
	if e != nil || !good || !canonicalEqual(lineage, x.Lineage) {
		return ErrConflict
	}
	var original bool
	e = u.tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM approved_results WHERE tenant_id=? AND product_id=? AND record_id=? AND task_id=? AND owner_subject_id=? AND status='COMMITTED')", p.scope.TenantID, p.scope.ProductID, p.result, p.task, p.scope.SubjectID).Scan(&original)
	if e != nil || !original {
		return fmt.Errorf("original approved result: %w", ErrDenied)
	}
	if p.kind == "TURN" {
		var body []byte
		var got domain.CandidateCommit
		e = u.tx.QueryRowContext(ctx, "SELECT document FROM harness_turn_commits WHERE tenant_id=? AND product_id=? AND task_id=? AND result_id=? AND receipt_key=?", p.scope.TenantID, p.scope.ProductID, p.task, p.result, p.receipt).Scan(&body)
		if e != nil || json.Unmarshal(body, &got) != nil || !reflect.DeepEqual(&got, x.Input.Turn) {
			return ErrConflict
		}
	}
	return nil
}
