package mysql

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"strconv"
	"time"
)

const noticeCursorDomain = "loreviva/NOTIFICATION_V1/1"

// Private plans contain original proof, not reusable caller permissions. Every
// transaction compares the actual SERVICE, current owner and full source again.
type notificationState struct {
	scope                             domain.Scope
	service                           currentIntegration
	eligible                          sessionEligibility
	consentRevision                   uint64
	versions                          domain.Versions
	subjectPrivacy, subjectView, high uint64
	status, consumer                  string
	catalog                           []withdrawalPublication
	keys                              []LockKey
}
type notificationBinding struct {
	Installation, Tenant, Product, Customer, Consumer, World, Service         string
	Generation, WorldPrivacy, WorldView, SubjectPrivacy, SubjectView, Consent uint64
	CustomerRevision, MappingRevision, EntitlementRevision, KeyVersion        uint64
}
type notificationCursor struct {
	Version             uint64
	Binding             notificationBinding
	Position, HighWater uint64
	Issued, Expires     time.Time
}
type notificationProof struct {
	Row                                    deliveryPageRow
	Initial                                *notificationProof
	Receipt                                receiptRow
	Original, Events                       []byte
	Notice, Kind, Summary, Message, Outbox string
	PayloadHash                            [32]byte
	Owner                                  string
}
type notificationPlan struct {
	store              *Store
	owner              DeliveryReaderOwner
	state              notificationState
	key                [32]byte
	cursor             notificationCursor
	inputCursor, exact string
	limit              uint16
	rows               []deliveryPageRow
	proofs             []notificationProof
	turns              map[string]*mediaSource
	controls           map[string]*integrationControlPlan
	keys               []LockKey
	acks               map[string]*notificationAck
}
type notificationAck struct {
	Key     string
	Hash    [32]byte
	Receipt generated.IntegrationReceipt
}
type notificationChallenge struct {
	Hash, Nonce       []byte
	Version, Revision uint64
	Expires           time.Time
}

func (s *Store) notificationOwner(ctx context.Context, owner DeliveryReaderOwner) error {
	s.integrationOwnerMu.Lock()
	same := owner != nil && owner == s.integrationDeliveryReader && owner.DeliveryReaderStore() == s
	s.integrationOwnerMu.Unlock()
	if !same || s.integrationClosed.Load() {
		return ErrDenied
	}
	return s.checkDeliveryAdmission(ctx)
}

func (s *Store) inspectNotificationState(ctx context.Context, q creationRows, world string) (notificationState, error) {
	var out notificationState
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || v.store != s || v.principal.Kind != domain.IntegrationService || domain.ValidateID(world) != nil {
		return out, ErrDenied
	}
	a, e := s.inspectIntegrationControl(ctx, q, v, world, "START", false, true)
	if e != nil {
		return out, fmt.Errorf("notification SERVICE/CREATE: %w", e)
	}
	p := v.principal
	out.service, out.eligible = v, a.eligibility
	out.scope = domain.Scope{ProductScope: domain.ProductScope{TenantID: p.Tenant, ProductID: p.Product, SubjectID: a.eligibility.entitlement.Player}, WorldInstanceID: world}
	// CONTROL safe metadata never fabricates a TURN-use prerequisite. This
	// revision is only a cursor/current stamp; each TURN row separately proves
	// its complete actual current choice and original accepted generation.
	if e = q.QueryRowContext(ctx, "SELECT COALESCE(MAX(revision),0) FROM integration_self_consent_current WHERE tenant_id=? AND product_id=? AND player_subject_id=?", p.Tenant, p.Product, out.scope.SubjectID).Scan(&out.consentRevision); e != nil {
		return out, e
	}
	out.keys = append([]LockKey{}, a.keys...)
	out.keys = append(out.keys, LockKey{Rank: 10, IDs: []string{p.Tenant}}, LockKey{Rank: 20, IDs: []string{p.Tenant, p.Product}}, LockKey{Rank: 40, IDs: []string{p.Tenant, p.Product, world}}, LockKey{Rank: 90, Kind: "integration_consent", IDs: []string{p.Tenant, p.Product, out.scope.SubjectID}})
	var enabled bool
	e = q.QueryRowContext(ctx, `SELECT g.privacy_epoch,g.view_epoch,g.enabled=1,w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,w.status,d.update_seq FROM world_controls w JOIN runtime_instances i USING(tenant_id,product_id,world_instance_id) JOIN delivery_streams d USING(tenant_id,product_id,world_instance_id) JOIN subject_governance g ON g.tenant_id=w.tenant_id AND g.product_id=w.product_id WHERE g.tenant_id=? AND g.product_id=? AND g.subject_id=? AND w.world_instance_id=? AND i.owner_subject_id=? AND i.mode='PRIVATE'`, p.Tenant, p.Product, out.scope.SubjectID, world, p.StableSubject).Scan(&out.subjectPrivacy, &out.subjectView, &enabled, &out.versions.ReleaseID, &out.versions.WorldRevision, &out.versions.ControlVersion, &out.versions.PrivacyEpoch, &out.versions.ViewEpoch, &out.status, &out.high)
	if e != nil {
		return out, fmt.Errorf("notification world snapshot: %w", e)
	}
	if !enabled {
		return out, ErrDenied
	}
	out.consumer = turnIdentity(p.Tenant, p.Product, p.Customer, "integration-consumer-v1")
	queue := []string{out.versions.ReleaseID}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		if len(seen) >= 200 {
			return out, ErrDenied
		}
		seen[id] = true
		var raw []byte
		var rel generated.Release
		if q.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", p.Tenant, p.Product, id).Scan(&raw) != nil || generated.Decode("Release", raw, &rel) != nil {
			return out, ErrDenied
		}
		sc := out.scope.ProductScope
		sc.SubjectID = string(rel.PublishedBySubjectID)
		pub, live, e := readWithdrawalPublication(ctx, q, sc, id)
		if e != nil || !live {
			return out, ErrDenied
		}
		out.catalog = append(out.catalog, pub)
		out.keys = append(out.keys, LockKey{Rank: 90, Kind: "content_release", IDs: []string{p.Tenant, p.Product, id}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{p.Tenant, p.Product, pub.result}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{p.Tenant, p.Product, string(pub.release.CheckReportID)}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{pub.result}})
		for _, d := range pub.release.DependencyLocks {
			queue = append(queue, string(d.ReleaseID))
		}
	}
	return out, nil
}
func (v notificationState) binding() notificationBinding {
	p := v.service.principal
	return notificationBinding{v.eligible.identity.Installation, p.Tenant, p.Product, p.Customer, v.consumer, v.scope.WorldInstanceID, p.PrincipalID, v.eligible.identity.Generation, v.versions.PrivacyEpoch, v.versions.ViewEpoch, v.subjectPrivacy, v.subjectView, v.consentRevision, v.eligible.mapping.CustomerRevision, v.eligible.mapping.MappingRevision, v.eligible.entitlement.Revision, 1}
}
func (p *notificationPlan) loadKey() error {
	if p.store.integrationSessionRoot == nil {
		return ErrDenied
	}
	material, e := p.store.integrationSessionRoot.SessionHMACKey(p.state.eligible.identity.Installation, false)
	if e != nil {
		return e
	}
	h := hmac.New(sha256.New, material[:])
	_, _ = h.Write([]byte(noticeCursorDomain))
	copy(p.key[:], h.Sum(nil))
	return nil
}
func (p *notificationPlan) openCursor(now time.Time) error {
	if p.inputCursor == "" {
		p.cursor = notificationCursor{Version: 1, Binding: p.state.binding(), HighWater: p.state.high, Issued: now, Expires: now.Add(15 * time.Minute)}
		return nil
	}
	if len(p.inputCursor) > 4096 {
		return ErrDenied
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(p.inputCursor)
	if e != nil {
		return ErrDenied
	}
	a, e := deliveryGCM(p.key)
	if e != nil || len(raw) < a.NonceSize()+a.Overhead() {
		return ErrDenied
	}
	plain, e := a.Open(nil, raw[:a.NonceSize()], raw[a.NonceSize():], []byte(noticeCursorDomain))
	if e != nil {
		return ErrDenied
	}
	d := json.NewDecoder(bytes.NewReader(plain))
	d.DisallowUnknownFields()
	if d.Decode(&p.cursor) != nil || d.Decode(new(any)) != io.EOF || p.cursor.Version != 1 || p.cursor.Binding != p.state.binding() || p.cursor.Position > p.cursor.HighWater || p.cursor.HighWater > p.state.high || p.cursor.Issued.After(now) || !p.cursor.Expires.Equal(p.cursor.Issued.Add(15*time.Minute)) || !now.Before(p.cursor.Expires) {
		return ErrDenied
	}
	// This private token has one producer spelling. A missing zero field or a
	// duplicate member must not silently acquire Go's decoder default value.
	closed, e := json.Marshal(p.cursor)
	if e != nil || !bytes.Equal(closed, plain) {
		return ErrDenied
	}
	return nil
}
func (p *notificationPlan) sealCursor(v notificationCursor) (string, error) {
	a, e := deliveryGCM(p.key)
	if e != nil {
		return "", e
	}
	n := make([]byte, a.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return "", e
	}
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(a.Seal(n, n, b, []byte(noticeCursorDomain))), nil
}

// Ordered initial updates, never opaque notice hashes, define pagination. An
// exact lookup is separately proved and does not change the stream ordering.
func (p *notificationPlan) readRows(ctx context.Context, q creationRows) ([]deliveryPageRow, error) {
	sc := p.state.scope
	query := `SELECT update_seq,business_update_id,kind,result_ref,task_ref,event_refs,packet_ref,delivery_state,resource_revision,audience_refs,lineage_refs FROM delivery_updates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND update_seq>? AND update_seq<=?`
	args := []any{sc.TenantID, sc.ProductID, sc.WorldInstanceID, p.cursor.Position, p.state.high}
	if p.exact != "" {
		query += ` AND kind IN ('CONTROL','TURN') AND result_ref=(SELECT result_ref FROM integration_notifications WHERE tenant_id=? AND product_id=? AND customer_id=? AND notice_ref=?)`
		args = append(args, sc.TenantID, sc.ProductID, p.state.service.principal.Customer, p.exact)
	}
	rows, e := q.QueryContext(ctx, query+" ORDER BY update_seq LIMIT 100", args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []deliveryPageRow{}
	last := p.cursor.Position
	visible := 0
	for rows.Next() {
		var r deliveryPageRow
		var ev, au, li []byte
		if e = rows.Scan(&r.Seq, &r.ID, &r.Kind, &r.Result, &r.Task, &ev, &r.Packet, &r.State, &r.Revision, &au, &li); e != nil {
			return nil, e
		}
		if json.Unmarshal(ev, &r.Events) != nil || json.Unmarshal(au, &r.Audience) != nil || json.Unmarshal(li, &r.Lineage) != nil {
			return nil, ErrDenied
		}
		if p.exact == "" && (r.Seq <= last || r.Seq-last != 1) {
			return nil, ErrDenied
		}
		last = r.Seq
		out = append(out, r)
		if r.Kind != "MEDIA" {
			visible++
		}
		if visible >= int(p.limit) {
			break
		}
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	if p.exact != "" && len(out) != 1 {
		return nil, ErrDenied
	}
	if p.exact == "" && visible < int(p.limit) && len(out) < 100 && last != p.state.high {
		return nil, ErrDenied
	}
	return out, nil
}

func (p *notificationPlan) controlOriginal(ctx context.Context, q creationRows, r deliveryPageRow) (*integrationControlPlan, error) {
	a := &integrationControlPlan{store: p.store, service: p.state.service, eligibility: p.state.eligible}
	var kind, principal string
	var issued time.Time
	if q.QueryRowContext(ctx, "SELECT r.document,i.principal_kind,i.principal_ref,i.issued_session_expires_at FROM runtime_tasks r JOIN integration_task_principals i USING(tenant_id,product_id,task_id) WHERE r.tenant_id=? AND r.product_id=? AND r.task_id=? AND i.task_kind='CONTROL'", p.state.scope.TenantID, p.state.scope.ProductID, r.Task).Scan(&a.raw, &kind, &principal, &issued) != nil || json.Unmarshal(a.raw, &a.document) != nil {
		return nil, ErrDenied
	}
	a.principal = p.state.service
	if kind == string(domain.IntegrationClient) {
		var key string
		if q.QueryRowContext(ctx, "SELECT x.original_key FROM integration_principals k JOIN integration_receipts x ON x.receipt_ref=k.issuance_receipt AND x.tenant_id=k.tenant_id AND x.product_id=k.product_id AND x.owner_id=k.issuer_service_key AND x.operation='EXCHANGE_SESSION' WHERE k.principal_id=? AND k.kind='CLIENT'", principal).Scan(&key) != nil {
			return nil, ErrDenied
		}
		original, e := sessionRead(ctx, q, p.state.service.principal, key)
		if e != nil || original.ID != principal || original.Player != p.state.scope.SubjectID || original.World != p.state.scope.WorldInstanceID || !original.Expires.Equal(issued) {
			return nil, ErrDenied
		}
		a.session = original
		a.principal.principal.Kind = domain.IntegrationClient
		a.principal.principal.PrincipalID = principal
		a.principal.principal.StableSubject = original.Player
		a.principal.principal.World = original.World
		a.principal.principal.ExpiresAt = original.Expires
		a.keys = append(a.keys, sessionKeys(p.state.service.principal, original, nil)...)
	} else if kind != string(domain.IntegrationService) || principal != p.state.service.principal.PrincipalID {
		return nil, ErrDenied
	}
	if a.document.Task != r.Task || a.document.Scope.WorldInstanceID != p.state.scope.WorldInstanceID || !integrationControlAction(a.principal.principal.Kind, a.document.Action) {
		return nil, ErrDenied
	}
	var e error
	a.hash, e = integrationControlHash(a.document)
	if e != nil {
		return nil, e
	}
	if e = a.checkOriginal(ctx, q, false); e != nil {
		return nil, e
	}
	a.keys = append(a.keys, harnessTaskKey(p.state.scope.ProductScope, r.Task), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{a.document.Receipt}})
	return a, nil
}

// Read and compare the original joint, its real task/COMMIT and both notice
// rows. Hidden MEDIA does not become a notice, but must retain its real proof.
func (p *notificationPlan) proof(ctx context.Context, q creationRows, r deliveryPageRow) (notificationProof, error) {
	out := notificationProof{Row: r}
	sc := p.state.scope
	if r.Kind == "MEDIA" {
		src := p.turns[r.Task]
		if src == nil {
			return out, ErrDenied
		}
		m := mediaRecord{State: r.State, Revision: r.Revision}
		if r.ID != mediaUpdateID(src, r.Revision, r.State) || r.Result != src.core.ResultID || !r.Packet.Valid || r.Packet.String != r.Result || len(r.Events) != 0 || !reflect.DeepEqual(r.Audience, []string{sc.SubjectID}) || !reflect.DeepEqual(r.Lineage, []string{src.core.Versions.ReleaseID}) || !((m.State == "PENDING" && m.Revision == 2) || ((m.State == "READY" || m.State == "FAILED") && m.Revision == 3) || (m.State == "WITHDRAWN" && (m.Revision == 3 || m.Revision == 4))) {
			return out, ErrDenied
		}
		raw, e := json.Marshal(src.core)
		if e != nil {
			return out, e
		}
		out.Original = raw
		// The cursor may already have passed the initial TURN. Resolve its
		// exact original identity, not a second stream page or a new notice.
		var initial deliveryPageRow
		var events, audience, lineage []byte
		id := turnIdentity(sc.TenantID, sc.ProductID, sc.WorldInstanceID, "TURN", r.Result, "1")
		e = q.QueryRowContext(ctx, `SELECT update_seq,business_update_id,kind,result_ref,task_ref,event_refs,packet_ref,delivery_state,resource_revision,audience_refs,lineage_refs FROM delivery_updates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND business_update_id=?`, sc.TenantID, sc.ProductID, sc.WorldInstanceID, id).Scan(&initial.Seq, &initial.ID, &initial.Kind, &initial.Result, &initial.Task, &events, &initial.Packet, &initial.State, &initial.Revision, &audience, &lineage)
		if e != nil || initial.Kind != "TURN" || initial.Task != r.Task || initial.Result != r.Result || initial.Seq == 0 || initial.Seq >= r.Seq || json.Unmarshal(events, &initial.Events) != nil || json.Unmarshal(audience, &initial.Audience) != nil || json.Unmarshal(lineage, &initial.Lineage) != nil {
			return out, ErrDenied
		}
		joint, e := p.proof(ctx, q, initial)
		if e != nil {
			return out, e
		}
		out.Initial = &joint
		return out, nil
	}
	if r.ID != turnIdentity(sc.TenantID, sc.ProductID, sc.WorldInstanceID, r.Kind, r.Result, "1") || r.Result != turnIdentity(r.Task, "result") || r.State != "COMMITTED" || r.Revision != 1 || !reflect.DeepEqual(r.Audience, []string{sc.SubjectID}) {
		return out, ErrDenied
	}
	key, op, command := r.Result, "INSTANCE_CONTROL", r.Task
	if r.Kind == "TURN" {
		key = turnIdentity(r.Task, "world-commit")
		op = "HARNESS_TURN_COMMIT"
		command = key
		src := p.turns[r.Task]
		if src == nil || !r.Packet.Valid || r.Packet.String != r.Result || !reflect.DeepEqual(r.Events, src.core.EventIDs) || !reflect.DeepEqual(r.Lineage, []string{src.core.Versions.ReleaseID}) {
			return out, ErrDenied
		}
		out.Owner = src.scope.SubjectID
		out.Original, _ = json.Marshal(src.core)
	} else if r.Kind == "CONTROL" {
		a, e := p.controlOriginal(ctx, q, r)
		if e != nil {
			return out, e
		}
		out.Owner = a.document.Scope.SubjectID
		out.Original = append([]byte(nil), a.raw...)
		if r.Packet.Valid || !reflect.DeepEqual(r.Events, []string{r.Result}) || !reflect.DeepEqual(r.Lineage, []string{sc.WorldInstanceID}) {
			return out, ErrDenied
		}
	} else {
		return out, ErrDenied
	}
	receipt, e := queryReceipt(ctx, q, key)
	if e != nil {
		return out, e
	}
	if receipt.tenant != sc.TenantID || receipt.product != sc.ProductID || receipt.owner != out.Owner || receipt.kind != "WORLD" || !receipt.world.Valid || receipt.world.String != sc.WorldInstanceID || receipt.task != r.Task || receipt.phase != "COMMIT" || receipt.operation != op || receipt.command != command || len(receipt.hash) != 32 {
		return out, ErrDenied
	}
	out.Receipt = receipt
	if r.Kind == "CONTROL" {
		h := sha256.Sum256(out.Original)
		if !bytes.Equal(h[:], receipt.hash) {
			return out, ErrDenied
		}
	}
	var valid bool
	var revision uint64
	if q.QueryRowContext(ctx, "SELECT status='COMMITTED',revision FROM approved_results WHERE tenant_id=? AND product_id=? AND record_id=? AND task_id=? AND owner_subject_id=?", sc.TenantID, sc.ProductID, r.Result, r.Task, out.Owner).Scan(&valid, &revision) != nil || !valid {
		return out, ErrDenied
	}
	events := []deliveryEventProof{}
	seen := map[string]bool{}
	for _, id := range r.Events {
		if seen[id] {
			return out, ErrDenied
		}
		seen[id] = true
		var ev deliveryEventProof
		if q.QueryRowContext(ctx, `SELECT e.record_id,e.tenant_id,e.product_id,e.owner_subject_id,e.task_id,e.world_instance_id,e.status,e.revision,r.document FROM events e JOIN runtime_events r USING(tenant_id,product_id,record_id) WHERE e.tenant_id=? AND e.product_id=? AND e.record_id=?`, sc.TenantID, sc.ProductID, id).Scan(&ev.ID, &ev.Tenant, &ev.Product, &ev.Owner, &ev.Task, &ev.World, &ev.Status, &ev.Revision, &ev.Raw) != nil {
			return out, ErrDenied
		}
		var k struct{ Kind, Action string }
		want := "CONTROL"
		if r.Kind == "TURN" {
			want = "ACTION_BUNDLE"
			revision = p.turns[r.Task].core.Versions.WorldRevision
		}
		if json.Unmarshal(ev.Raw, &k) != nil || k.Kind != want || ev.Owner != out.Owner || ev.Task != r.Task || ev.World != sc.WorldInstanceID || ev.Status != "COMMITTED" || ev.Revision != revision {
			return out, ErrDenied
		}
		if r.Kind == "CONTROL" && k.Action != p.controls[r.Task].document.Action {
			return out, ErrDenied
		}
		events = append(events, ev)
	}
	out.Events, _ = json.Marshal(events)
	out.Notice = turnIdentity(p.state.consumer, sc.WorldInstanceID, r.Result, "1")
	out.Message = out.Notice
	out.Outbox = turnIdentity(out.Notice, "integration-outbox")
	out.Kind = "CONTROL_COMMITTED"
	out.Summary = "SYNTHETIC_CONTROL_COMMITTED"
	if r.Kind == "TURN" {
		out.Kind = "RESULT_COMMITTED"
		out.Summary = "SYNTHETIC_RESULT_COMMITTED"
	}
	payload, e := deliveryJSON(struct {
		Kind, Summary, Notice, Result, Message string
		Revision                               uint64
	}{out.Kind, out.Summary, out.Notice, r.Result, out.Message, 1})
	if e != nil {
		return out, e
	}
	out.PayloadHash = sha256.Sum256(payload)
	var lineage []byte
	ack, e := p.readAck(ctx, q, out)
	if e != nil {
		return out, e
	}
	wantState := "PENDING"
	wantRevision := uint64(1)
	if ack != nil {
		wantState = "ACKED"
		wantRevision = 2
	}
	e = q.QueryRowContext(ctx, `SELECT n.consumer_id=? AND n.world_instance_id=? AND n.result_ref=? AND n.initial_revision=1 AND n.kind=? AND n.payload_hash=? AND n.message_ref=? AND n.outbox_ref=? AND o.task_id=? AND o.owner_subject_id=? AND o.consumer_id=? AND o.message_id=? AND o.status=? AND o.revision=?,n.visibility_source_refs FROM integration_notifications n JOIN outbox o ON o.tenant_id=n.tenant_id AND o.product_id=n.product_id AND o.record_id=n.outbox_ref WHERE n.tenant_id=? AND n.product_id=? AND n.customer_id=? AND n.notice_ref=?`, p.state.consumer, sc.WorldInstanceID, r.Result, out.Kind, out.PayloadHash[:], out.Message, out.Outbox, r.Task, out.Owner, p.state.consumer, out.Message, wantState, wantRevision, sc.TenantID, sc.ProductID, p.state.service.principal.Customer, out.Notice).Scan(&valid, &lineage)
	var refs []string
	if e != nil || !valid || json.Unmarshal(lineage, &refs) != nil || !reflect.DeepEqual(refs, r.Lineage) {
		return out, ErrDenied
	}
	return out, nil
}

func (s *Store) prepareNotifications(ctx context.Context, owner DeliveryReaderOwner, world, notice, cursor string, limit uint16) (*notificationPlan, error) {
	if e := s.notificationOwner(ctx, owner); e != nil {
		return nil, e
	}
	if limit == 0 {
		limit = 20
	}
	if limit > 100 || notice != "" && (domain.ValidateID(notice) != nil || cursor != "") {
		return nil, ErrDenied
	}
	p := &notificationPlan{store: s, owner: owner, limit: limit, exact: notice, inputCursor: cursor, turns: map[string]*mediaSource{}, controls: map[string]*integrationControlPlan{}, acks: map[string]*notificationAck{}}
	var e error
	p.state, e = s.inspectNotificationState(ctx, s.DB, world)
	if e != nil {
		return nil, fmt.Errorf("notification state: %w", e)
	}
	if e = p.loadKey(); e != nil {
		return nil, e
	}
	var now time.Time
	if e = s.DB.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return nil, e
	}
	if e = p.openCursor(now); e != nil {
		return nil, e
	}
	p.rows, e = p.readRows(ctx, s.DB)
	if e != nil {
		return nil, e
	}
	p.keys = append([]LockKey{}, p.state.keys...)
	for _, r := range p.rows {
		if r.Kind == "TURN" || r.Kind == "MEDIA" {
			if p.turns[r.Task] == nil {
				src, e := s.mediaSource(ctx, s.DB, p.state.scope.TenantID, p.state.scope.ProductID, r.Task)
				if e != nil {
					return nil, e
				}
				if src.turn.service.principal.PrincipalID != p.state.service.principal.PrincipalID || src.scope != p.state.scope {
					return nil, ErrDenied
				}
				if e = s.planMedia(ctx, src); e != nil {
					return nil, e
				}
				p.turns[r.Task] = src
				p.keys = append(p.keys, baseLocks(src.request)...)
				p.keys = append(p.keys, src.request.Locks...)
			}
		}
		if r.Kind == "CONTROL" {
			a, e := p.controlOriginal(ctx, s.DB, r)
			if e != nil {
				return nil, fmt.Errorf("notification original CONTROL: %w", e)
			}
			p.controls[r.Task] = a
			p.keys = append(p.keys, a.keys...)
		}
		proof, e := p.proof(ctx, s.DB, r)
		if e != nil {
			return nil, fmt.Errorf("notification original %s joint: %w", r.Kind, e)
		}
		p.proofs = append(p.proofs, proof)
		joint := proof
		if proof.Initial != nil {
			joint = *proof.Initial
			p.keys = append(p.keys, LockKey{Rank: 110, Kind: "delivery_update", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, joint.Row.ID}})
		}
		ack, e := p.readAck(ctx, s.DB, joint)
		if e != nil {
			return nil, e
		}
		p.acks[joint.Notice] = ack
		if ack != nil {
			p.keys = append(p.keys, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, p.state.consumer, ack.Key}})
		}
		p.keys = append(p.keys, LockKey{Rank: 110, Kind: "delivery_update", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, r.ID}})
		// Hidden MEDIA takes the same original joint locks and ACK proof, but
		// never enters challenge creation or notice output below.
		p.keys = append(p.keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{joint.Receipt.key}}, LockKey{Rank: 110, Kind: "approved_results", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, joint.Row.Result}}, LockKey{Rank: 110, Kind: "integration_notice", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, joint.Notice}}, LockKey{Rank: 110, Kind: "outbox", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, joint.Outbox}})
		if r.Kind != "MEDIA" {
			p.keys = append(p.keys, LockKey{Rank: 90, Kind: "integration_challenge", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, p.state.service.principal.Customer, p.state.consumer, proof.Notice, "1"}})
		}
		for _, id := range joint.Row.Events {
			p.keys = append(p.keys, LockKey{Rank: 110, Kind: "events", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, id}})
		}
	}
	return p, nil
}

// One closed transaction owns the precollected set. The existing source unit
// remains bound to its real original task, even when several notices are read.
func (p *notificationPlan) begin(ctx context.Context) (*sql.Tx, func(), error) {
	ctx, installation, release, e := p.store.WorldAccountingAuthority.EnterWorldAccounting(ctx)
	if e != nil {
		return nil, nil, e
	}
	keys, e := PlanLocks(p.keys)
	if e != nil {
		release()
		return nil, nil, e
	}
	tx, e := p.store.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		release()
		return nil, nil, e
	}
	done := func() { _ = tx.Rollback(); release() }
	fail := func(e error) (*sql.Tx, func(), error) { done(); return nil, nil, e }
	for _, src := range p.turns {
		src.request.approvedInstallation = &installation
	}
	for _, k := range keys {
		// Existing generation keys keep their original owning acquisition rules.
		var source *mediaSource
		for _, src := range p.turns {
			for _, x := range append(baseLocks(src.request), src.request.Locks...) {
				if reflect.DeepEqual(x, k) {
					source = src
					break
				}
			}
			if source != nil {
				break
			}
		}
		if e = marker(ctx, tx, k); e != nil {
			return fail(e)
		}
		switch k.Kind {
		case "delivery_update":
			e = studioLockRows(ctx, tx, "SELECT business_update_id FROM delivery_updates WHERE tenant_id=? AND product_id=? AND business_update_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "integration_notice":
			e = studioLockRows(ctx, tx, "SELECT notice_ref FROM integration_notifications WHERE tenant_id=? AND product_id=? AND notice_ref=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "outbox":
			e = studioLockRows(ctx, tx, "SELECT record_id FROM outbox WHERE tenant_id=? AND product_id=? AND record_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "integration_challenge":
			e = studioLockRows(ctx, tx, "SELECT notice_ref FROM integration_challenges WHERE tenant_id=? AND product_id=? AND customer_id=? AND consumer_id=? AND notice_ref=? AND initial_revision=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3], k.IDs[4], k.IDs[5])
		case "integration_receipt":
			e = studioLockRows(ctx, tx, "SELECT receipt_ref FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND original_key=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3])
		case "integration_participant":
			e = studioLockRows(ctx, tx, "SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "integration_consent":
			e = studioLockRows(ctx, tx, "SELECT player_subject_id FROM integration_self_consent_current WHERE tenant_id=? AND product_id=? AND player_subject_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "delivery_media":
			e = studioLockRows(ctx, tx, "SELECT asset_id FROM delivery_media WHERE tenant_id=? AND product_id=? AND asset_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "events", "approved_results":
			e = studioLockRows(ctx, tx, "SELECT record_id FROM "+k.Kind+" WHERE tenant_id=? AND product_id=? AND record_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
		case "commit_receipts":
			e = studioLockRows(ctx, tx, "SELECT commit_key FROM commit_receipts WHERE commit_key=? FOR UPDATE", k.IDs[0])
		case "content_check":
			e = studioLockRows(ctx, tx, "SELECT result_id FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			if e == nil {
				e = studioLockRows(ctx, tx, "SELECT report_id FROM content_checks WHERE tenant_id=? AND product_id=? AND report_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			}
		default:
			if k.Rank == 40 {
				for _, table := range []string{"world_controls", "runtime_instances", "delivery_streams"} {
					e = studioLockRows(ctx, tx, "SELECT world_instance_id FROM "+table+" WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
					if e != nil {
						break
					}
				}
			} else if source != nil && k.Kind != "integration_principal" && k.Kind != "integration_entitlement" && k.Kind != "integration_customer" && k.Kind != "integration_mapping" {
				e = p.store.acquire(ctx, tx, source.request, k)
			} else if k.Rank == 50 {
				for _, table := range []string{"tasks", "runtime_tasks", "harness_turns", "harness_turn_commits"} {
					e = studioLockRows(ctx, tx, "SELECT task_id FROM "+table+" WHERE tenant_id=? AND product_id=? AND task_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
					if e != nil {
						break
					}
				}
			} else {
				e = lockStudioRows(ctx, tx, k)
			}
		}
		if e != nil {
			return fail(fmt.Errorf("notification planned lock %d/%s: %w", k.Rank, k.Kind, e))
		}
	}
	return tx, done, nil
}
func (p *notificationPlan) current(ctx context.Context, tx *sql.Tx) error {
	if e := p.store.notificationOwner(ctx, p.owner); e != nil {
		return e
	}
	state, e := p.store.inspectNotificationState(ctx, tx, p.state.scope.WorldInstanceID)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(state, p.state) {
		return ErrReplan
	}
	rows, e := p.readRows(ctx, tx)
	if e != nil || !reflect.DeepEqual(rows, p.rows) {
		return ErrDenied
	}
	keys, e := PlanLocks(p.keys)
	if e != nil {
		return e
	}
	plan := map[string]bool{}
	for _, k := range keys {
		plan[string(lockBytes(k))] = true
	}
	for _, src := range p.turns {
		u := &unit{tx: tx, plan: plan, scope: src.scope.ProductScope, world: src.scope.WorldInstanceID, taskID: src.task, commandOnly: true, approvedHistoryRead: true, operatorStore: p.store, operation: "HARNESS_APPROVED_READ", purpose: ExecutionPurpose, controlVersion: src.request.Versions.ControlVersion, integrationTurn: src.turn}
		if e = p.store.checkMediaSource(ctx, u, src); e != nil {
			return e
		}
	}
	for i, r := range p.rows {
		proof, e := p.proof(ctx, tx, r)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(proof, p.proofs[i]) {
			return ErrDenied
		}
		joint := proof
		if proof.Initial != nil {
			joint = *proof.Initial
		}
		ack, e := p.readAck(ctx, tx, joint)
		if e != nil || !reflect.DeepEqual(ack, p.acks[joint.Notice]) {
			return ErrDenied
		}
	}
	var now time.Time
	if tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now) != nil || !now.Before(p.cursor.Expires) {
		return ErrDenied
	}
	return ctx.Err()
}
func (p *notificationPlan) challenge(ctx context.Context, tx *sql.Tx, notice string, create bool) (notificationChallenge, error) {
	var c notificationChallenge
	v := p.state.service.principal
	e := tx.QueryRowContext(ctx, "SELECT nonce_hash,nonce,key_version,expires_at,revision FROM integration_challenges WHERE tenant_id=? AND product_id=? AND customer_id=? AND consumer_id=? AND notice_ref=? AND initial_revision=1", v.Tenant, v.Product, v.Customer, p.state.consumer, notice).Scan(&c.Hash, &c.Nonce, &c.Version, &c.Expires, &c.Revision)
	exists := e == nil
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return c, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return c, e
	}
	if exists {
		h := sha256.Sum256(c.Nonce)
		if len(c.Nonce) != 32 || !bytes.Equal(c.Hash, h[:]) || c.Version != 1 || c.Revision == 0 {
			return c, ErrDenied
		}
		if now.Before(c.Expires) {
			return c, nil
		}
	}
	if !create {
		return c, ErrDenied
	}
	if c.Revision == ^uint64(0) {
		return c, ErrConflict
	}
	c.Nonce = make([]byte, 32)
	if _, e = rand.Read(c.Nonce); e != nil {
		return c, e
	}
	h := sha256.Sum256(c.Nonce)
	c.Hash = append([]byte(nil), h[:]...)
	c.Version = 1
	c.Expires = now.Add(120 * time.Second)
	c.Revision++
	var result sql.Result
	if exists {
		result, e = tx.ExecContext(ctx, "UPDATE integration_challenges SET nonce_hash=?,nonce=?,key_version=1,expires_at=?,revision=? WHERE tenant_id=? AND product_id=? AND customer_id=? AND consumer_id=? AND notice_ref=? AND initial_revision=1 AND revision=?", c.Hash, c.Nonce, c.Expires, c.Revision, v.Tenant, v.Product, v.Customer, p.state.consumer, notice, c.Revision-1)
	} else {
		result, e = tx.ExecContext(ctx, "INSERT INTO integration_challenges VALUES(?,?,?,?,?,1,?,?,1,?,1)", v.Tenant, v.Product, v.Customer, p.state.consumer, notice, c.Hash, c.Nonce, c.Expires)
	}
	if e != nil {
		return c, e
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return c, ErrConflict
	}
	return c, nil
}
func (p *notificationPlan) challengeWire(notice string, c notificationChallenge) generated.IntegrationChallenge {
	ref := turnIdentity(p.state.consumer, notice, "1", "challenge", strconv.FormatUint(c.Revision, 10), hex.EncodeToString(c.Hash))
	return generated.IntegrationChallenge{ChallengeRef: generated.Identity(ref), ConsumerRef: generated.Identity(p.state.consumer), NotificationRef: generated.Identity(notice), NotificationRevision: "1", Nonce: base64.RawURLEncoding.EncodeToString(c.Nonce), ExpiresAt: c.Expires.UTC().Format(time.RFC3339Nano)}
}
func (s *Store) GetIntegrationNotifications(ctx context.Context, owner DeliveryReaderOwner, world, notice, cursor string, limit uint16) (generated.IntegrationNotificationPage, error) {
	var zero generated.IntegrationNotificationPage
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	p, e := s.prepareNotifications(ctx, owner, world, notice, cursor, limit)
	if e != nil {
		return zero, fmt.Errorf("notification precollection: %w", e)
	}
	tx, done, e := p.begin(ctx)
	if e != nil {
		return zero, e
	}
	defer done()
	if e = p.current(ctx, tx); e != nil {
		return zero, fmt.Errorf("notification current: %w", e)
	}
	out := generated.IntegrationNotificationPage{Notifications: []generated.IntegrationNotification{}, Scope: generated.WorldScope{TenantID: p.state.scope.TenantID, ProductID: p.state.scope.ProductID, WorldInstanceID: p.state.scope.WorldInstanceID}}
	expected := map[string]notificationChallenge{}
	for _, proof := range p.proofs {
		if proof.Row.Kind == "MEDIA" {
			continue
		}
		c, e := p.challenge(ctx, tx, proof.Notice, true)
		if e != nil {
			return zero, e
		}
		expected[proof.Notice] = c
		receipt := json.RawMessage("null")
		if ack := p.acks[proof.Notice]; ack != nil {
			receipt, _ = json.Marshal(ack.Receipt)
		}
		out.Notifications = append(out.Notifications, generated.IntegrationNotification{Challenge: p.challengeWire(proof.Notice, c), ConsumerRef: generated.Identity(p.state.consumer), Kind: proof.Kind, MessageRef: generated.Identity(proof.Message), NotificationRef: generated.Identity(proof.Notice), NotificationRevision: "1", PayloadHash: hex.EncodeToString(proof.PayloadHash[:]), Receipt: receipt, SafeSummary: proof.Summary, Scope: out.Scope})
	}
	next := p.cursor
	next.HighWater = p.state.high
	if len(p.rows) > 0 {
		next.Position = p.rows[len(p.rows)-1].Seq
	}
	if p.inputCursor != "" && next == p.cursor {
		out.NextCursor = p.inputCursor
	} else {
		out.NextCursor, e = p.sealCursor(next)
		if e != nil {
			return zero, e
		}
	}
	raw, e := json.Marshal(out)
	if e != nil {
		return zero, e
	}
	if e = generated.Validate("IntegrationNotificationPage", raw); e != nil {
		return zero, fmt.Errorf("notification projection: %w", e)
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return zero, e
		}
	}
	if e = p.current(ctx, tx); e != nil {
		return zero, e
	}
	for id, c := range expected {
		after, e := p.challenge(ctx, tx, id, false)
		if e != nil || !reflect.DeepEqual(c, after) {
			return zero, ErrDenied
		}
	}
	if e = tx.Commit(); e != nil {
		return zero, e
	}
	return out, nil
}

const notificationReceiptOperation = "recordCustomerNotificationReceipt"

// Customer evidence, original notification payload, and platform idempotency
// have separate hash domains. Neither a hash nor a nonce grants authority.
func (p *notificationPlan) messageHash(v notificationProof) string {
	b, _ := json.Marshal([7]string{p.state.eligible.identity.Installation, p.state.service.principal.Customer, p.state.consumer, v.Notice, "1", v.Message, v.Summary})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (p *notificationPlan) receiptHash(v notificationProof) [32]byte {
	b, _ := json.Marshal([11]string{notificationReceiptOperation, p.state.eligible.identity.Installation, p.state.scope.TenantID, p.state.scope.ProductID, p.state.service.principal.Customer, p.state.consumer, v.Notice, "1", v.Message, p.messageHash(v), "PERSISTED:" + hex.EncodeToString(v.PayloadHash[:])})
	return sha256.Sum256(b)
}
func (p *notificationPlan) receiptRef(v notificationProof, key string) string {
	return turnIdentity(p.state.scope.TenantID, p.state.scope.ProductID, p.state.consumer, notificationReceiptOperation, v.Notice, key)
}
func (p *notificationPlan) readAck(ctx context.Context, q creationRows, v notificationProof) (*notificationAck, error) {
	rows, e := q.QueryContext(ctx, `SELECT original_key,receipt_ref,payload_hash,result_metadata,state,target_revision,committed_at FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND operation=? AND scope_kind='NOTIFICATION' AND scope_address=? LIMIT 2`, p.state.scope.TenantID, p.state.scope.ProductID, p.state.consumer, notificationReceiptOperation, v.Notice)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out *notificationAck
	for rows.Next() {
		if out != nil {
			return nil, ErrDenied
		}
		a := &notificationAck{}
		var ref, state string
		var hash, raw []byte
		var revision uint64
		var committed time.Time
		if e = rows.Scan(&a.Key, &ref, &hash, &raw, &state, &revision, &committed); e != nil {
			return nil, e
		}
		if domain.ValidateIdempotencyKey(a.Key) != nil || generated.Decode("IntegrationReceipt", raw, &a.Receipt) != nil || state != "COMMITTED" || revision != 1 {
			return nil, ErrDenied
		}
		a.Hash = p.receiptHash(v)
		want := generated.IntegrationReceipt{ConsumerRef: generated.Identity(p.state.consumer), CustomerMessageHash: p.messageHash(v), CustomerMessageRef: generated.Identity(v.Message), CustomerPersistenceEvidence: "AUTHENTICATED_CUSTOMER_STATEMENT", NotificationRef: generated.Identity(v.Notice), NotificationRevision: "1", PayloadHash: hex.EncodeToString(v.PayloadHash[:]), ReceiptRef: generated.Identity(p.receiptRef(v, a.Key)), RecordedAt: committed.UTC().Format(time.RFC3339Nano), State: "RECORDED"}
		if ref != string(want.ReceiptRef) || !bytes.Equal(hash, a.Hash[:]) || a.Receipt != want {
			return nil, ErrDenied
		}
		out = a
	}
	return out, rows.Err()
}

// The existing SERVICE is rechecked before both first writes and replay. Fresh
// expectations come from the actual write arguments, never a post-hook sample.
func (s *Store) RecordIntegrationNotificationReceipt(ctx context.Context, owner DeliveryReaderOwner, notice, key string, input generated.IntegrationReceiptRequest) (generated.IntegrationReceipt, error) {
	var zero generated.IntegrationReceipt
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, e := json.Marshal(input)
	if e != nil || generated.Validate("IntegrationReceiptRequest", raw) != nil || domain.ValidateID(notice) != nil || domain.ValidateIdempotencyKey(key) != nil {
		return zero, ErrDenied
	}
	if e = s.notificationOwner(ctx, owner); e != nil {
		return zero, e
	}
	vctx, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || vctx.store != s || vctx.principal.Kind != domain.IntegrationService {
		return zero, ErrDenied
	}
	principal := vctx.principal
	var world string
	e = s.DB.QueryRowContext(ctx, "SELECT world_instance_id FROM integration_notifications WHERE tenant_id=? AND product_id=? AND customer_id=? AND consumer_id=? AND notice_ref=?", principal.Tenant, principal.Product, principal.Customer, turnIdentity(principal.Tenant, principal.Product, principal.Customer, "integration-consumer-v1"), notice).Scan(&world)
	if e != nil {
		return zero, ErrDenied
	}
	p, e := s.prepareNotifications(ctx, owner, world, notice, "", 1)
	if e != nil {
		return zero, e
	}
	if len(p.proofs) != 1 || p.proofs[0].Notice != notice || p.proofs[0].Row.Kind == "MEDIA" {
		return zero, ErrDenied
	}
	v := p.proofs[0]
	if input.NotificationRevision != "1" || string(input.CustomerMessageRef) != v.Message || input.CustomerStatus != "PERSISTED" || input.CustomerMessageHash != p.messageHash(v) || input.PayloadHash != hex.EncodeToString(v.PayloadHash[:]) {
		return zero, ErrConflict
	}
	p.keys = append(p.keys, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{p.state.scope.TenantID, p.state.scope.ProductID, p.state.consumer, key}})
	tx, done, e := p.begin(ctx)
	if e != nil {
		return zero, e
	}
	defer done()
	if e = p.current(ctx, tx); e != nil {
		return zero, e
	}
	c, e := p.challenge(ctx, tx, notice, false)
	if e != nil {
		return zero, e
	}
	challenge := p.challengeWire(notice, c)
	if input.ChallengeRef != challenge.ChallengeRef || !hmac.Equal([]byte(input.ChallengeNonce), []byte(challenge.Nonce)) {
		return zero, ErrDenied
	}
	a := p.acks[notice]
	if a != nil && a.Key != key {
		return zero, ErrConflict
	}
	if a == nil {
		var now time.Time
		if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return zero, e
		}
		a = &notificationAck{Key: key, Hash: p.receiptHash(v), Receipt: generated.IntegrationReceipt{ConsumerRef: generated.Identity(p.state.consumer), CustomerMessageHash: p.messageHash(v), CustomerMessageRef: generated.Identity(v.Message), CustomerPersistenceEvidence: "AUTHENTICATED_CUSTOMER_STATEMENT", NotificationRef: generated.Identity(notice), NotificationRevision: "1", PayloadHash: hex.EncodeToString(v.PayloadHash[:]), ReceiptRef: generated.Identity(p.receiptRef(v, key)), RecordedAt: now.UTC().Format(time.RFC3339Nano), State: "RECORDED"}}
		body, e := json.Marshal(a.Receipt)
		if e != nil || generated.Validate("IntegrationReceipt", body) != nil {
			return zero, ErrDenied
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO integration_receipts VALUES(?,?,?,?,?,?,?,?,?,CAST(? AS JSON),'COMMITTED',1,?)", p.state.scope.TenantID, p.state.scope.ProductID, p.state.consumer, notificationReceiptOperation, "NOTIFICATION", notice, key, string(a.Receipt.ReceiptRef), a.Hash[:], string(body), now)
		if e != nil {
			return zero, e
		}
		r, e := tx.ExecContext(ctx, "UPDATE outbox SET status='ACKED',revision=2 WHERE tenant_id=? AND product_id=? AND record_id=? AND status='PENDING' AND revision=1", p.state.scope.TenantID, p.state.scope.ProductID, v.Outbox)
		if e != nil {
			return zero, e
		}
		n, e := r.RowsAffected()
		if e != nil || n != 1 {
			return zero, ErrConflict
		}
		p.acks[notice] = a
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return zero, e
		}
	}
	if e = p.current(ctx, tx); e != nil {
		return zero, e
	}
	after, e := p.challenge(ctx, tx, notice, false)
	if e != nil || !reflect.DeepEqual(c, after) {
		return zero, ErrDenied
	}
	if e = tx.Commit(); e != nil {
		return zero, e
	}
	return a.Receipt, nil
}
