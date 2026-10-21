package mysql

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"reflect"
	"strconv"
	"sync"
	"time"
)

// These interfaces expose only data checks by the original bound Files owner.
// Neither a returned Store pointer nor an interface implementation mints a seal.
type RuntimeMediaEvidence interface{ RuntimeMediaEvidenceStore() *Store }
type RuntimeMediaFinalPin interface{ RuntimeMediaFinalPinStore() *Store }
type RuntimeMediaFileOwner interface {
	RuntimeMediaFileStore() *Store
	CheckMediaFinalEvidencePure(*RuntimeMediaClaim, RuntimeMediaFinalPin) (RuntimeMediaFileFacts, error)
}
type RuntimeMediaStagePlan struct {
	OriginalStorage, StageStorage, Asset string
	Fence                                uint64
	Scope                                domain.ProductScope
}
type RuntimeMediaFileFacts struct {
	Asset, Storage, SHA256, NativeIdentity string
	Bytes, Fence                           uint64
}
type RuntimeMediaFinalDisposition string

const (
	MediaFinalUnknown      RuntimeMediaFinalDisposition = "UNKNOWN"
	MediaFinalCommitted    RuntimeMediaFinalDisposition = "COMMITTED"
	MediaFinalNotCommitted RuntimeMediaFinalDisposition = "NOT_COMMITTED"
)

type mediaSource struct {
	scope                     domain.Scope
	task, actor, asset, media string
	turn                      *integrationTurnPlan
	core                      domain.CandidateCommit
	request                   BarrierRequest
	write                     *mediaWrite
	retirement                *mediaRetirementProof
}
type mediaWrite struct {
	tx       *sql.Tx
	unit     *unit
	source   *mediaSource
	h        uint64
	appended bool
	id       string
	state    string
	revision uint64
}
type mediaRecord struct {
	Asset, World, Packet, Media, Storage, Hash, MIME, State, Result, LeaseOwner string
	Sources                                                                     []byte
	Bytes, Revision, Fence, Attempts, Generation                                uint64
	Accepted, Deadline, Lease                                                   time.Time
	LeaseValid, OwnerValid                                                      bool
}
type RuntimeMediaClaim struct {
	self            *RuntimeMediaClaim
	store           *Store
	owner           DeliveryReaderOwner
	files           RuntimeMediaFileOwner
	source          *mediaSource
	expected        mediaRecord
	plan            RuntimeMediaStagePlan
	mu              sync.Mutex
	operation       sync.Mutex
	closed          bool
	disposition     RuntimeMediaFinalDisposition
	finalExpected   *mediaRecord
	rolledBack      bool
	finalTerminated bool
}

func (c *RuntimeMediaClaim) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.self != c {
		return ErrDenied
	}
	c.closed = true
	return nil
}

func (s *Store) BindRuntimeMediaOwner(owner DeliveryReaderOwner, f RuntimeMediaFileOwner) error {
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if owner == nil || owner != s.integrationDeliveryReader || owner.DeliveryReaderStore() != s || f == nil || f.RuntimeMediaFileStore() != s || any(f) != any(s.ApprovedTurnReader) || any(f) != any(s.OriginalResultReader) || s.integrationMediaOwner != nil {
		return ErrDenied
	}
	s.integrationMediaOwner = f
	return nil
}
func (s *Store) mediaOwner(ctx context.Context, owner DeliveryReaderOwner) (RuntimeMediaFileOwner, error) {
	s.integrationOwnerMu.Lock()
	f := s.integrationMediaOwner
	same := owner != nil && owner == s.integrationDeliveryReader && owner.DeliveryReaderStore() == s
	s.integrationOwnerMu.Unlock()
	if !same || f == nil || f.RuntimeMediaFileStore() != s || any(f) != any(s.ApprovedTurnReader) || any(f) != any(s.OriginalResultReader) || s.integrationClosed.Load() {
		return nil, ErrDenied
	}
	if e := s.checkDeliveryAdmission(ctx); e != nil {
		return nil, e
	}
	return f, nil
}

// Resolve the persisted issuer only as historical/current proof. This does not
// install an HTTP principal in ctx and never creates a general body capability.
func (s *Store) mediaSource(ctx context.Context, q creationRows, tenant, product, task string) (*mediaSource, error) {
	var service, world, player, originalSession, exchange string
	e := q.QueryRowContext(ctx, `SELECT i.issuer_service_key,i.world_instance_id,i.player_subject_id,i.principal_ref,x.original_key FROM integration_task_principals i JOIN integration_principals p ON p.principal_id=i.principal_ref JOIN integration_receipts x ON x.receipt_ref=p.issuance_receipt AND x.tenant_id=p.tenant_id AND x.product_id=p.product_id AND x.owner_id=p.issuer_service_key AND x.operation='EXCHANGE_SESSION' WHERE i.tenant_id=? AND i.product_id=? AND i.task_id=? AND i.task_kind='TURN' AND i.principal_kind='CLIENT'`, tenant, product, task).Scan(&service, &world, &player, &originalSession, &exchange)
	if e != nil {
		return nil, e
	}
	v, e := s.integrationServiceProof(ctx, q, service)
	if e != nil {
		return nil, fmt.Errorf("media service proof: %w", e)
	}
	// The once-bound media owner selected this original committed task. This
	// local-only context lets the existing issuer validator compare that exact
	// persisted proof; it never escapes as a SERVICE/CLIENT caller capability.
	ctx = context.WithValue(ctx, integrationContextKey{}, v)
	a, e := s.inspectIntegrationTurn(ctx, q, v, world, false)
	if e != nil || a.scope.SubjectID != player {
		return nil, fmt.Errorf("media current turn qualification: %w", ErrDenied)
	}
	a.session, e = sessionRead(ctx, q, v.principal, exchange)
	if e != nil || a.session.ID != originalSession {
		return nil, fmt.Errorf("media original session: %w", ErrDenied)
	}
	a.task = task
	var hash, raw []byte
	var actor string
	e = q.QueryRowContext(ctx, "SELECT h.original_key,h.quote_id,h.command_hash,h.actor_id,c.document FROM harness_turns h JOIN harness_turn_commits c USING(tenant_id,product_id,task_id) WHERE h.tenant_id=? AND h.product_id=? AND h.task_id=? AND h.owner_subject_id=? AND h.world_instance_id=?", tenant, product, task, player, world).Scan(&a.key, &a.quote, &hash, &actor, &raw)
	if e != nil || len(hash) != 32 || task != turnIdentity(tenant, product, player, world, "TURN", a.key) {
		return nil, fmt.Errorf("media original command identity: %w", ErrDenied)
	}
	copy(a.hash[:], hash)
	if e = a.verifyIntake(ctx, q, a.intakeRequest(), task, true, true); e != nil {
		return nil, fmt.Errorf("media intake proof: %w", e)
	}
	var core domain.CandidateCommit
	if json.Unmarshal(raw, &core) != nil || core.ResultID != turnIdentity(task, "result") || core.ReceiptKey != turnIdentity(task, "world-commit") || core.Metadata.Identity.Scope != a.scope || core.Metadata.Identity.TaskID != task {
		return nil, fmt.Errorf("media original result tuple: %w", ErrDenied)
	}
	a.keys = append(a.keys, LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{originalSession}}, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{tenant, product, service, exchange}}, harnessTaskKey(a.scope.ProductScope, task), harnessTaskKey(a.scope.ProductScope, a.quote), LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(task, "accepted")}})
	out := &mediaSource{scope: a.scope, task: task, actor: actor, turn: a, core: core, media: turnIdentity(tenant, product, world, core.ResultID, "AUDIO"), asset: turnIdentity(tenant, product, world, core.ResultID, "AUDIO", "asset")}
	return out, nil
}
func (s *Store) planMedia(ctx context.Context, p *mediaSource) error {
	sc := p.scope
	var status string
	var v domain.Versions
	var g GateVersions
	var enabled bool
	e := s.DB.QueryRowContext(ctx, "SELECT w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,w.status,t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,t.enabled AND p.enabled AND g.enabled AND g.can_execute FROM world_controls w JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) JOIN subject_governance g ON g.tenant_id=w.tenant_id AND g.product_id=w.product_id AND g.subject_id=? WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=?", sc.SubjectID, sc.TenantID, sc.ProductID, sc.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status, &g.Tenant, &g.Product, &g.Subject, &g.Privacy, &g.View, &enabled)
	if e != nil || !enabled {
		return ErrDenied
	}
	p.request = BarrierRequest{approvedHistoryRead: true, ExpectedWorldStatus: status, ProductScope: sc.ProductScope, WorldScope: &p.scope, Versions: &v, OwnerSubjectID: sc.SubjectID, Operation: "HARNESS_APPROVED_READ", commandOnly: true, TaskID: p.task, GateVersions: g, Locks: append([]LockKey{}, p.turn.keys...)}
	p.request.CommandKey = turnIdentity(p.task, "approved-read")
	p.request.CommitKey = p.request.CommandKey
	p.request.Purpose = ExecutionPurpose
	p.request.integrationTurn = p.turn
	rows, e := s.DB.QueryContext(ctx, "SELECT a.attempt_id,z.authorization_id FROM attempts a JOIN dispatch_authorizations z USING(tenant_id,product_id,task_id,attempt_id) WHERE a.tenant_id=? AND a.product_id=? AND a.task_id=? ORDER BY a.slot LIMIT 3", sc.TenantID, sc.ProductID, p.task)
	if e != nil {
		return e
	}
	count := 0
	for rows.Next() {
		var attempt, authorization string
		if e = rows.Scan(&attempt, &authorization); e != nil {
			rows.Close()
			return e
		}
		count++
		p.request.Locks = append(p.request.Locks, LockKey{Rank: 60, IDs: []string{sc.TenantID, sc.ProductID, p.task, attempt, authorization}})
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if count < 1 || count > 2 {
		return ErrDenied
	}
	if e = s.planCommittedHistory(ctx, &p.request); e != nil {
		return e
	}
	// Use the verified original creation graph, not the legacy subject-owned
	// runtime lookup. This private plan carries no task lease or send right.
	if e = s.planPerception(context.WithValue(ctx, integrationTurnKey{}, p.turn), &p.request, p.actor); e != nil {
		return e
	}
	p.request.Locks = append(p.request.Locks, LockKey{Rank: 90, Kind: "delivery_media", IDs: []string{sc.TenantID, sc.ProductID, p.asset}})
	// Freeze all possible next transition addresses before the transaction. The
	// current media row and world lock serialize selecting exactly one of them.
	for rev := uint64(2); rev <= 5; rev++ {
		for _, state := range []string{"PENDING", "READY", "FAILED", "WITHDRAWN"} {
			p.request.Locks = append(p.request.Locks, LockKey{Rank: 110, Kind: "delivery_update", IDs: []string{sc.TenantID, sc.ProductID, mediaUpdateID(p, rev, state)}})
		}
	}
	return nil
}
func mediaUpdateID(p *mediaSource, rev uint64, state string) string {
	return turnIdentity(p.scope.WorldInstanceID, p.core.ResultID, p.core.ResultID, p.media, strconv.FormatUint(rev, 10), state)
}

// The unit is private, minted only under this owning media operation's real
// shared admission, original history plan and sorted locks. No Tx escapes it.
func (s *Store) mediaTx(ctx context.Context, p *mediaSource) (*unit, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ctx, installation, done, e := s.WorldAccountingAuthority.EnterWorldAccounting(ctx)
	if e != nil {
		cancel()
		return nil, nil, e
	}
	end := func() { done(); cancel() }
	p.request.approvedInstallation = &installation
	keys, e := PlanLocks(append(baseLocks(p.request), p.request.Locks...))
	if e != nil {
		end()
		return nil, nil, e
	}
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if e != nil {
		end()
		return nil, nil, e
	}
	u := &unit{tx: tx, plan: map[string]bool{}, scope: p.scope.ProductScope, world: p.scope.WorldInstanceID, taskID: p.task, commandOnly: true, approvedHistoryRead: true, operatorStore: s, operation: "HARNESS_APPROVED_READ", purpose: ExecutionPurpose, controlVersion: p.request.Versions.ControlVersion}
	u.integrationTurn = p.turn
	fail := func(e error) (*unit, func(), error) { _ = tx.Rollback(); end(); return nil, nil, e }
	for _, k := range keys {
		if p.retirement != nil {
			handled, lockErr := s.mediaRetirementLock(ctx, tx, p, k)
			if handled {
				if lockErr != nil {
					return fail(lockErr)
				}
				u.plan[string(lockBytes(k))] = true
				if s.Trace != nil {
					s.Trace(k)
				}
				continue
			}
		}
		switch {
		case k.Kind == "delivery_media":
			e = marker(ctx, tx, k)
			if e == nil {
				e = studioLockRows(ctx, tx, "SELECT asset_id FROM delivery_media WHERE tenant_id=? AND product_id=? AND asset_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			}
		case k.Kind == "delivery_update":
			e = marker(ctx, tx, k)
			if e == nil {
				e = studioLockRows(ctx, tx, "SELECT business_update_id FROM delivery_updates WHERE tenant_id=? AND product_id=? AND business_update_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			}
		case k.Kind == "integration_receipt":
			e = marker(ctx, tx, k)
			if e == nil {
				e = studioLockRows(ctx, tx, "SELECT receipt_ref FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND original_key=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2], k.IDs[3])
			}
		case k.Kind == "integration_participant":
			e = marker(ctx, tx, k)
			if e == nil {
				e = studioLockRows(ctx, tx, "SELECT world_instance_id FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			}
		case k.Kind == "integration_consent":
			e = marker(ctx, tx, k)
			if e == nil {
				e = studioLockRows(ctx, tx, "SELECT player_subject_id FROM integration_self_consent_current WHERE tenant_id=? AND product_id=? AND player_subject_id=? FOR UPDATE", k.IDs[0], k.IDs[1], k.IDs[2])
			}
		case k.Kind == "integration_principal" || k.Kind == "integration_entitlement" || k.Kind == "integration_customer" || k.Kind == "integration_mapping":
			e = marker(ctx, tx, k)
			if e == nil {
				e = lockStudioRows(ctx, tx, k)
			}
		default:
			e = s.acquire(ctx, tx, p.request, k)
		}
		if e != nil {
			return fail(fmt.Errorf("media precollected lock %d/%s: %w", k.Rank, k.Kind, e))
		}
		u.plan[string(lockBytes(k))] = true
		if s.Trace != nil {
			s.Trace(k)
		}
	}
	return u, func() { _ = tx.Rollback(); end() }, nil
}
func (s *Store) checkMediaSource(ctx context.Context, u *unit, p *mediaSource) error {
	if e := s.checkDeliveryAdmission(ctx); e != nil {
		return e
	}
	var v domain.Versions
	var gates GateVersions
	var status string
	var enabled bool
	e := u.tx.QueryRowContext(ctx, "SELECT w.release_id,w.world_revision,w.control_version,w.privacy_epoch,w.view_epoch,w.status,t.permission_version,p.permission_version,g.permission_version,g.privacy_epoch,g.view_epoch,t.enabled AND p.enabled AND g.enabled AND g.can_execute FROM world_controls w JOIN tenant_gates t USING(tenant_id) JOIN product_gates p USING(tenant_id,product_id) JOIN subject_governance g ON g.tenant_id=w.tenant_id AND g.product_id=w.product_id AND g.subject_id=? WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=?", p.scope.SubjectID, p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status, &gates.Tenant, &gates.Product, &gates.Subject, &gates.Privacy, &gates.View, &enabled)
	if e != nil || !enabled || v != *p.request.Versions || gates != p.request.GateVersions || status != p.request.ExpectedWorldStatus {
		return ErrDenied
	}
	n, e := s.mediaSource(ctx, u.tx, p.scope.TenantID, p.scope.ProductID, p.task)
	if e != nil || n.scope != p.scope || !reflect.DeepEqual(n.core, p.core) || !reflect.DeepEqual(n.turn.session, p.turn.session) || !reflect.DeepEqual(n.turn.eligibility, p.turn.eligibility) || !reflect.DeepEqual(n.turn.service, p.turn.service) || !reflect.DeepEqual(n.turn.choice, p.turn.choice) {
		return ErrDenied
	}
	current, e := s.GenerationContext.InspectApprovedTurnTx(ctx, u, p.scope, p.task, *p.request.Versions)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(current, p.core) {
		return ErrDenied
	}
	costs := []domain.MockResultMetadata{current.Metadata}
	if current.OriginalDependency != nil {
		costs = append(costs, *current.OriginalDependency)
	}
	if !allOriginalUsageKnown(costs) {
		if e = checkOriginalServiceMeteringTx(ctx, u, p.scope.ProductScope, p.task, turnIdentity(p.task, "service"), turnIdentity(p.task, "world-commit"), costs); e != nil {
			return e
		}
	}
	return ctx.Err()
}
func readMedia(ctx context.Context, q creationRows, p *mediaSource) (mediaRecord, error) {
	var m mediaRecord
	var storage, hash, mime, owner sql.NullString
	var bytes sql.NullInt64
	var lease sql.NullTime
	e := q.QueryRowContext(ctx, `SELECT asset_id,world_instance_id,packet_id,media_id,runtime_file_ref,HEX(content_hash),byte_length,mime_type,state,revision,original_result_ref,source_refs,lease_owner,fence,lease_expires_at,attempt_count,accepted_at,execute_by,maintenance_generation FROM delivery_media WHERE tenant_id=? AND product_id=? AND asset_id=?`, p.scope.TenantID, p.scope.ProductID, p.asset).Scan(&m.Asset, &m.World, &m.Packet, &m.Media, &storage, &hash, &bytes, &mime, &m.State, &m.Revision, &m.Result, &m.Sources, &owner, &m.Fence, &lease, &m.Attempts, &m.Accepted, &m.Deadline, &m.Generation)
	m.Storage = storage.String
	m.Hash = hash.String
	m.MIME = mime.String
	m.Bytes = uint64(bytes.Int64)
	m.LeaseOwner = owner.String
	m.Lease = lease.Time
	m.LeaseValid = lease.Valid
	m.OwnerValid = owner.Valid
	return m, e
}
func (s *Store) appendMediaUpdate(ctx context.Context, u *unit, p *mediaSource, m mediaRecord) error {
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
	id := mediaUpdateID(p, m.Revision, m.State)
	if e := u.RequireLock(LockKey{Rank: 110, Kind: "delivery_update", IDs: []string{p.scope.TenantID, p.scope.ProductID, id}}); e != nil {
		return e
	}
	audience, _ := deliveryJSON([]string{p.scope.SubjectID})
	lineage, _ := deliveryJSON([]string{p.core.Versions.ReleaseID})
	_, e := u.tx.ExecContext(ctx, "INSERT INTO delivery_updates VALUES(?,?,?,?,?,'MEDIA',?,?,?, ?,?,?,?,?)", p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, h+1, id, p.core.ResultID, p.task, "[]", p.core.ResultID, m.State, m.Revision, string(audience), string(lineage))
	if e != nil {
		return e
	}
	res, e := u.tx.ExecContext(ctx, "UPDATE delivery_streams SET update_seq=? WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND update_seq=?", h+1, p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, h)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return ErrConflict
	}
	p.write = &mediaWrite{tx: u.tx, unit: u, source: p, h: h + 1, appended: true, id: id, state: m.State, revision: m.Revision}
	return nil
}
func (s *Store) mediaFinal(ctx context.Context, u *unit, p *mediaSource, m mediaRecord) error {
	if s.BeforeCommit != nil {
		if e := s.BeforeCommit(ctx, u.tx); e != nil {
			return e
		}
	}
	if e := s.checkMediaSource(ctx, u, p); e != nil {
		return e
	}
	actual, e := readMedia(ctx, u.tx, p)
	if e != nil || !reflect.DeepEqual(actual, m) {
		return ErrConflict
	}
	w := p.write
	if w == nil || w.tx != u.tx || w.unit != u || w.source != p {
		return ErrDenied
	}
	var h uint64
	if e = u.tx.QueryRowContext(ctx, "SELECT update_seq FROM delivery_streams WHERE tenant_id=? AND product_id=? AND world_instance_id=?", p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID).Scan(&h); e != nil || h != w.h {
		return ErrConflict
	}
	if w.appended {
		var valid bool
		var events, audience, lineage []byte
		e = u.tx.QueryRowContext(ctx, "SELECT business_update_id=? AND kind='MEDIA' AND result_ref=? AND task_ref=? AND packet_ref=? AND delivery_state=? AND resource_revision=?,event_refs,audience_refs,lineage_refs FROM delivery_updates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND update_seq=?", w.id, p.core.ResultID, p.task, p.core.ResultID, w.state, w.revision, p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, w.h).Scan(&valid, &events, &audience, &lineage)
		var ev, au, li []string
		if e != nil || !valid || json.Unmarshal(events, &ev) != nil || len(ev) != 0 || json.Unmarshal(audience, &au) != nil || !reflect.DeepEqual(au, []string{p.scope.SubjectID}) || json.Unmarshal(lineage, &li) != nil || !reflect.DeepEqual(li, []string{p.core.Versions.ReleaseID}) {
			return ErrConflict
		}
	}
	return nil
}
func (s *Store) ClaimSyntheticMedia(ctx context.Context, owner DeliveryReaderOwner) (*RuntimeMediaClaim, error) {
	f, e := s.mediaOwner(ctx, owner)
	if e != nil {
		return nil, e
	}
	// Only the original committed integration TURN is eligible. CONTROL has no
	// packet; no caller chooses tenant, subject, result or original storage.
	var tenant, product, task string
	e = s.DB.QueryRowContext(ctx, `SELECT h.tenant_id,h.product_id,h.task_id FROM harness_turn_commits h JOIN integration_task_principals i USING(tenant_id,product_id,task_id) LEFT JOIN delivery_media m ON m.tenant_id=h.tenant_id AND m.product_id=h.product_id AND m.packet_id=h.result_id WHERE i.task_kind='TURN' AND (m.asset_id IS NULL OR (m.state='PENDING' AND m.lease_expires_at<=UTC_TIMESTAMP(6))) ORDER BY h.tenant_id,h.product_id,h.task_id LIMIT 1`).Scan(&tenant, &product, &task)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, ErrNoTask
	}
	if e != nil {
		return nil, e
	}
	p, e := s.mediaSource(ctx, s.DB, tenant, product, task)
	if e != nil {
		return nil, fmt.Errorf("media original source: %w", e)
	}
	if e = s.planMedia(ctx, p); e != nil {
		return nil, fmt.Errorf("media current planning: %w", e)
	}
	u, done, e := s.mediaTx(ctx, p)
	if e != nil {
		return nil, e
	}
	defer done()
	if e = s.checkMediaSource(ctx, u, p); e != nil {
		return nil, fmt.Errorf("media claim current: %w", e)
	}
	var now time.Time
	var generation uint64
	if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6),generation FROM memory_installation").Scan(&now, &generation); e != nil {
		return nil, e
	}
	m, e := readMedia(ctx, u.tx, p)
	if errors.Is(e, sql.ErrNoRows) {
		source, _ := deliveryJSON([]string{p.core.Versions.ReleaseID})
		m = mediaRecord{Asset: p.asset, World: p.scope.WorldInstanceID, Packet: p.core.ResultID, Media: p.media, State: "PENDING", Revision: 2, Result: p.core.ResultID, Sources: source, LeaseOwner: turnIdentity(p.asset, "owner", "1"), LeaseValid: true, OwnerValid: true, Fence: 1, Attempts: 1, Accepted: now, Deadline: now.Add(60 * time.Second), Lease: now.Add(30 * time.Second), Generation: generation}
		_, e = u.tx.ExecContext(ctx, `INSERT INTO delivery_media VALUES(?,?,?,?,?,?,NULL,NULL,NULL,NULL,'PENDING',2,?,?,?,1,?,1,?,?,?)`, tenant, product, m.Asset, m.World, m.Packet, m.Media, m.Result, string(m.Sources), m.LeaseOwner, m.Lease, m.Accepted, m.Deadline, m.Generation)
		if e == nil {
			e = s.appendMediaUpdate(ctx, u, p, m)
		}
	} else if e == nil {
		// Validate the existing row before it can become this writer's expected
		// tuple. Natural expiry permits reclaim, never repair of a malformed row.
		if m.Generation != generation || validateMediaShape(p.scope.ProductScope, p.scope.WorldInstanceID, p.core.ResultID, []string{p.core.Versions.ReleaseID}, m) != nil {
			return nil, ErrDenied
		}
		var h uint64
		if e = u.tx.QueryRowContext(ctx, "SELECT update_seq FROM delivery_streams WHERE tenant_id=? AND product_id=? AND world_instance_id=?", tenant, product, p.scope.WorldInstanceID).Scan(&h); e != nil {
			return nil, e
		}
		p.write = &mediaWrite{tx: u.tx, unit: u, source: p, h: h}
		if m.State != "PENDING" || m.Lease.After(now) {
			return nil, ErrNoTask
		}
		if !m.Deadline.After(now) || m.Attempts >= 2 {
			m.State = "FAILED"
			m.Revision++
			_, e = u.tx.ExecContext(ctx, "UPDATE delivery_media SET state='FAILED',revision=? WHERE tenant_id=? AND product_id=? AND asset_id=? AND state='PENDING' AND fence=?", m.Revision, tenant, product, m.Asset, m.Fence)
			if e == nil {
				e = s.appendMediaUpdate(ctx, u, p, m)
			}
		} else {
			m.Fence++
			m.Attempts++
			m.LeaseOwner = turnIdentity(m.Asset, "owner", strconv.FormatUint(m.Fence, 10))
			m.Lease = now.Add(30 * time.Second)
			if m.Lease.After(m.Deadline) {
				m.Lease = m.Deadline
			}
			_, e = u.tx.ExecContext(ctx, "UPDATE delivery_media SET fence=?,attempt_count=?,lease_owner=?,lease_expires_at=? WHERE tenant_id=? AND product_id=? AND asset_id=?", m.Fence, m.Attempts, m.LeaseOwner, m.Lease, tenant, product, m.Asset)
		}
	}
	if e != nil {
		return nil, e
	}
	if e = s.mediaFinal(ctx, u, p, m); e != nil {
		return nil, fmt.Errorf("media claim final: %w", e)
	}
	commit := s.Commit
	if commit == nil {
		commit = func(_ context.Context, t *sql.Tx) error { return t.Commit() }
	}
	if e = commit(ctx, u.tx); e != nil {
		return nil, ErrCommitUnknown
	}
	if m.State != "PENDING" {
		return nil, ErrNoTask
	}
	c := &RuntimeMediaClaim{store: s, owner: owner, files: f, source: p, expected: m, disposition: MediaFinalUnknown}
	c.self = c
	c.plan = RuntimeMediaStagePlan{OriginalStorage: p.core.Metadata.BodyStorage, StageStorage: turnIdentity(p.asset, "runtime-media", strconv.FormatUint(m.Fence, 10)), Asset: p.asset, Fence: m.Fence, Scope: p.scope.ProductScope}
	return c, nil
}
func (s *Store) claimValid(c *RuntimeMediaClaim, owner DeliveryReaderOwner, allowClosed bool) error {
	if c == nil || c.self != c || c.store != s || c.owner != owner || c.files != s.integrationMediaOwner || c.source == nil {
		return ErrDenied
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed && !allowClosed {
		return ErrDenied
	}
	return nil
}
func (s *Store) RuntimeMediaStagePlan(ctx context.Context, f RuntimeMediaFileOwner, c *RuntimeMediaClaim) (RuntimeMediaStagePlan, error) {
	if c == nil || f != s.integrationMediaOwner || f != c.files || s.claimValid(c, c.owner, false) != nil {
		return RuntimeMediaStagePlan{}, ErrDenied
	}
	c.operation.Lock()
	defer c.operation.Unlock()
	u, done, e := s.mediaTx(ctx, c.source)
	if e != nil {
		return RuntimeMediaStagePlan{}, e
	}
	defer done()
	if e = s.checkMediaSource(ctx, u, c.source); e != nil {
		return RuntimeMediaStagePlan{}, e
	}
	m, e := readMedia(ctx, u.tx, c.source)
	var live bool
	if e != nil || !reflect.DeepEqual(m, c.expected) {
		return RuntimeMediaStagePlan{}, ErrDenied
	}
	e = u.tx.QueryRowContext(ctx, "SELECT ? > UTC_TIMESTAMP(6) AND ? > UTC_TIMESTAMP(6)", m.Lease, m.Deadline).Scan(&live)
	if e != nil || !live {
		return RuntimeMediaStagePlan{}, ErrDenied
	}
	return c.plan, nil
}
func (s *Store) FinishSyntheticMedia(ctx context.Context, owner DeliveryReaderOwner, c *RuntimeMediaClaim, pin RuntimeMediaFinalPin) error {
	if s.claimValid(c, owner, false) != nil {
		return ErrDenied
	}
	c.operation.Lock()
	defer c.operation.Unlock()
	f, e := s.mediaOwner(ctx, owner)
	if e != nil {
		return e
	}
	facts, e := f.CheckMediaFinalEvidencePure(c, pin)
	if e != nil {
		return e
	}
	hash, e := hex.DecodeString(facts.SHA256)
	if e != nil || len(hash) != 32 || facts.Asset != c.plan.Asset || facts.Storage != c.plan.StageStorage || facts.Fence != c.plan.Fence || facts.Bytes != 160044 || facts.NativeIdentity == "" {
		return ErrDenied
	}
	u, done, e := s.mediaTx(ctx, c.source)
	if e != nil {
		return e
	}
	c.mu.Lock()
	c.disposition = MediaFinalUnknown
	c.rolledBack = false
	c.finalTerminated = false
	c.mu.Unlock()
	defer func() {
		// Original termination and exact absence are separate evidence. A
		// successful rollback alone must not publish NOT_COMMITTED.
		rollbackErr := u.tx.Rollback()
		if rollbackErr == nil {
			c.mu.Lock()
			c.rolledBack = true
			c.mu.Unlock()
		}
		if rollbackErr == nil || errors.Is(rollbackErr, sql.ErrTxDone) {
			c.mu.Lock()
			c.finalTerminated = true
			c.mu.Unlock()
		}
		done()
	}()
	if e = s.checkMediaSource(ctx, u, c.source); e != nil {
		return e
	}
	old, e := readMedia(ctx, u.tx, c.source)
	if e != nil || !reflect.DeepEqual(old, c.expected) {
		return ErrDenied
	}
	var valid bool
	e = u.tx.QueryRowContext(ctx, "SELECT ? > UTC_TIMESTAMP(6) AND ? > UTC_TIMESTAMP(6) AND generation=? FROM memory_installation", old.Lease, old.Deadline, old.Generation).Scan(&valid)
	if e != nil || !valid {
		return ErrDenied
	}
	m := old
	m.State = "READY"
	m.Revision++
	m.Storage = facts.Storage
	m.Hash = fmt.Sprintf("%X", hash)
	m.Bytes = facts.Bytes
	m.MIME = "audio/wav"
	c.mu.Lock()
	copyExpected := m
	c.finalExpected = &copyExpected
	c.mu.Unlock()
	res, e := u.tx.ExecContext(ctx, "UPDATE delivery_media SET state='READY',revision=?,runtime_file_ref=?,content_hash=?,byte_length=?,mime_type='audio/wav' WHERE tenant_id=? AND product_id=? AND asset_id=? AND state='PENDING' AND fence=? AND lease_owner=? AND lease_expires_at>UTC_TIMESTAMP(6) AND execute_by>UTC_TIMESTAMP(6)", m.Revision, m.Storage, hash, m.Bytes, c.source.scope.TenantID, c.source.scope.ProductID, m.Asset, m.Fence, m.LeaseOwner)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n != 1 {
		return ErrDenied
	}
	if e = s.appendMediaUpdate(ctx, u, c.source, m); e != nil {
		return e
	}
	if e = s.mediaFinal(ctx, u, c.source, m); e != nil {
		return e
	}
	final, e := f.CheckMediaFinalEvidencePure(c, pin)
	if e != nil || final != facts {
		return ErrDenied
	}
	if e = s.claimValid(c, owner, false); e != nil {
		return e
	}
	e = u.tx.QueryRowContext(ctx, "SELECT ? > UTC_TIMESTAMP(6) AND ? > UTC_TIMESTAMP(6)", old.Lease, old.Deadline).Scan(&valid)
	if e != nil || !valid {
		return ErrDenied
	}
	c.mu.Lock()
	c.disposition = MediaFinalUnknown
	c.mu.Unlock()
	commit := s.Commit
	if commit == nil {
		commit = func(_ context.Context, t *sql.Tx) error { return t.Commit() }
	}
	if e = commit(ctx, u.tx); e != nil {
		return ErrCommitUnknown
	}
	c.mu.Lock()
	c.disposition = MediaFinalCommitted
	c.mu.Unlock()
	return nil
}
func (s *Store) MediaFinalOutcome(ctx context.Context, f RuntimeMediaFileOwner, c *RuntimeMediaClaim) (RuntimeMediaFinalDisposition, error) {
	if c == nil || f != s.integrationMediaOwner || f != c.files || s.claimValid(c, c.owner, true) != nil {
		return MediaFinalUnknown, ErrDenied
	}
	if !c.operation.TryLock() {
		return MediaFinalUnknown, nil
	}
	defer c.operation.Unlock()
	c.mu.Lock()
	d := c.disposition
	expected := c.finalExpected
	rolledBack := c.rolledBack
	terminated := c.finalTerminated
	c.mu.Unlock()
	if d != MediaFinalUnknown {
		return d, nil
	}
	// Reconciliation uses one short SQL snapshot after the original transaction
	// has ended. No Files operation or authority renewal occurs here.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, e := s.DB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if e != nil {
		return MediaFinalUnknown, e
	}
	defer tx.Rollback()
	m, e := readMedia(ctx, tx, c.source)
	if e != nil {
		return MediaFinalUnknown, e
	}
	if expected == nil || !terminated {
		return MediaFinalUnknown, nil
	}
	p := c.source
	var valid bool
	var events, audience, lineage []byte
	e = tx.QueryRowContext(ctx, "SELECT kind='MEDIA' AND world_instance_id=? AND result_ref=? AND task_ref=? AND packet_ref=? AND delivery_state=? AND resource_revision=?,event_refs,audience_refs,lineage_refs FROM delivery_updates WHERE tenant_id=? AND product_id=? AND business_update_id=?", p.scope.WorldInstanceID, p.core.ResultID, p.task, p.core.ResultID, expected.State, expected.Revision, p.scope.TenantID, p.scope.ProductID, mediaUpdateID(p, expected.Revision, expected.State)).Scan(&valid, &events, &audience, &lineage)
	if errors.Is(e, sql.ErrNoRows) && rolledBack && reflect.DeepEqual(m, c.expected) {
		c.mu.Lock()
		c.disposition = MediaFinalNotCommitted
		c.mu.Unlock()
		return MediaFinalNotCommitted, nil
	}
	if e != nil {
		return MediaFinalUnknown, e
	}
	var ev, au, li []string
	if !valid || json.Unmarshal(events, &ev) != nil || len(ev) != 0 || json.Unmarshal(audience, &au) != nil || !reflect.DeepEqual(au, []string{p.scope.SubjectID}) || json.Unmarshal(lineage, &li) != nil || !reflect.DeepEqual(li, []string{p.core.Versions.ReleaseID}) {
		return MediaFinalUnknown, ErrConflict
	}
	retained, proofErr := s.mediaRetained(ctx, tx, p.asset)
	if proofErr != nil {
		return MediaFinalUnknown, proofErr
	}
	if retained.source.scope != p.scope || retained.source.task != p.task || !reflect.DeepEqual(retained.source.core, p.core) || !reflect.DeepEqual(retained.record, m) {
		return MediaFinalUnknown, ErrConflict
	}
	matched := reflect.DeepEqual(m, *expected)
	if m.State == "WITHDRAWN" && m.Revision == expected.Revision+1 && expected.State == "READY" {
		// Only retirement state/revision and the exact permanent intent may differ.
		// Retained history above must include the original READY before WITHDRAWN.
		copyCurrent := m
		copyCurrent.State = expected.State
		copyCurrent.Revision = expected.Revision
		if m.LeaseOwner == mediaPurgeIntent(p.scope.ProductScope, m) && !m.LeaseValid {
			copyCurrent.LeaseOwner = expected.LeaseOwner
			copyCurrent.Lease = expected.Lease
			copyCurrent.LeaseValid = expected.LeaseValid
		}
		matched = reflect.DeepEqual(copyCurrent, *expected)
	}
	if matched {
		c.mu.Lock()
		c.disposition = MediaFinalCommitted
		c.mu.Unlock()
		return MediaFinalCommitted, nil
	}
	return MediaFinalUnknown, nil
}

// RuntimeMediaRead is a CLIENT read, never a producer claim or worker lease.
// The complete original/current source and CLIENT proof share each short Tx.
type RuntimeMediaRead struct {
	self             *RuntimeMediaRead
	store            *Store
	owner            DeliveryReaderOwner
	files            RuntimeMediaFileOwner
	principal        currentIntegration
	client           *integrationTurnPlan
	source           *mediaSource
	expected         mediaRecord
	closed, finished bool
}

type RuntimeMediaReadPlan struct {
	Scope                               domain.ProductScope
	Asset, Media, Storage, SHA256, MIME string
	Bytes, Revision                     uint64
}

func (s *Store) OpenRuntimeMediaRead(ctx context.Context, owner DeliveryReaderOwner, asset string) (*RuntimeMediaRead, error) {
	f, e := s.mediaOwner(ctx, owner)
	if e != nil {
		return nil, e
	}
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	if !ok || v.store != s || v.principal.Kind != domain.IntegrationClient || domain.ValidateID(asset) != nil {
		return nil, ErrDenied
	}
	var task string
	e = s.DB.QueryRowContext(ctx, "SELECT c.task_id FROM delivery_media m JOIN harness_turn_commits c ON c.tenant_id=m.tenant_id AND c.product_id=m.product_id AND c.result_id=m.original_result_ref WHERE m.tenant_id=? AND m.product_id=? AND m.world_instance_id=? AND m.asset_id=?", v.principal.Tenant, v.principal.Product, v.principal.World, asset).Scan(&task)
	if e != nil {
		return nil, ErrDenied
	}
	p, e := s.mediaSource(ctx, s.DB, v.principal.Tenant, v.principal.Product, task)
	if e != nil || p.asset != asset || p.scope.SubjectID != v.principal.StableSubject || p.scope.WorldInstanceID != v.principal.World {
		return nil, ErrDenied
	}
	a, e := s.integrationTaskPlan(ctx, s.DB, task)
	if e != nil {
		return nil, e
	}
	a.action = "APPROVED_READ"
	if e = a.planApprovedChoice(ctx, s.DB); e != nil {
		return nil, e
	}
	if e = s.planMedia(ctx, p); e != nil {
		return nil, e
	}
	p.request.Locks = append(p.request.Locks, a.keys...)
	r := &RuntimeMediaRead{store: s, owner: owner, files: f, principal: v, client: a, source: p}
	r.self = r
	m, e := r.current(ctx)
	if e != nil {
		return nil, fmt.Errorf("media read initial current: %w", e)
	}
	r.expected = m
	return r, nil
}

func (r *RuntimeMediaRead) current(ctx context.Context) (mediaRecord, error) {
	var zero mediaRecord
	if r == nil || r.self != r || r.closed || ctx.Value(integrationContextKey{}) != r.principal {
		return zero, ErrDenied
	}
	if _, e := r.store.mediaOwner(ctx, r.owner); e != nil {
		return zero, e
	}
	u, done, e := r.store.mediaTx(ctx, r.source)
	if e != nil {
		return zero, e
	}
	defer done()
	check := func() error {
		if e := r.store.checkMediaSource(ctx, u, r.source); e != nil {
			return e
		}
		request := r.source.request
		request.integrationTurn = r.client
		return r.store.checkIntegrationApprovedRead(context.WithValue(ctx, integrationTurnKey{}, r.client), u.tx, request)
	}
	if e = check(); e != nil {
		return zero, fmt.Errorf("media read current before: %w", e)
	}
	m, e := readMedia(ctx, u.tx, r.source)
	if e != nil {
		return zero, e
	}
	// Committed playback permits natural producer-lease expiry, but retains
	// every original shape invariant, including non-NULL and accepted bounds.
	if validateMediaShape(r.source.scope.ProductScope, r.source.scope.WorldInstanceID, r.source.core.ResultID, []string{r.source.core.Versions.ReleaseID}, m) != nil {
		return zero, ErrDenied
	}
	if m.State != "READY" || m.Storage != turnIdentity(m.Asset, "runtime-media", strconv.FormatUint(m.Fence, 10)) || m.Generation != r.source.request.approvedInstallation.Generation {
		return zero, ErrDenied
	}
	if r.store.BeforeReadFinish != nil {
		if e = r.store.BeforeReadFinish(ctx, u.tx); e != nil {
			return zero, e
		}
	}
	if e = check(); e != nil {
		return zero, fmt.Errorf("media read current final: %w", e)
	}
	after, e := readMedia(ctx, u.tx, r.source)
	if e != nil || !reflect.DeepEqual(after, m) {
		return zero, ErrReplan
	}
	return m, nil
}

func (r *RuntimeMediaRead) FilePlan(f RuntimeMediaFileOwner) (RuntimeMediaReadPlan, error) {
	if r == nil || r.self != r || r.closed || f != r.files || f != r.store.integrationMediaOwner {
		return RuntimeMediaReadPlan{}, ErrDenied
	}
	m := r.expected
	return RuntimeMediaReadPlan{Scope: r.source.scope.ProductScope, Asset: m.Asset, Media: m.Media, Storage: m.Storage, SHA256: m.Hash, MIME: m.MIME, Bytes: m.Bytes, Revision: m.Revision}, nil
}

func (r *RuntimeMediaRead) MatchFinished(old *RuntimeMediaRead) error {
	if r == nil || r.self != r || r.closed || old == nil || old.self != old || !old.closed || !old.finished || r.store != old.store || r.owner != old.owner || r.files != old.files || r.principal != old.principal || !reflect.DeepEqual(r.expected, old.expected) || !reflect.DeepEqual(r.source.core, old.source.core) {
		return ErrReplan
	}
	return nil
}
func (r *RuntimeMediaRead) Finish(ctx context.Context) error {
	if r == nil || r.self != r || r.closed {
		return ErrDenied
	}
	defer r.Close()
	m, e := r.current(ctx)
	if e != nil {
		return e
	}
	if !reflect.DeepEqual(m, r.expected) {
		return ErrReplan
	}
	r.finished = true
	return nil
}
func (r *RuntimeMediaRead) Close() error {
	if r == nil {
		return nil
	}
	if r.self != r {
		return ErrDenied
	}
	r.closed = true
	return nil
}

// Historical MEDIA identity is proved from its immutable update and original
// TURN COMMIT. Latest file/fence/state are intentionally absent from this proof.
func (r *IntegrationRead) mediaRowProof(q withdrawalQuerier, v deliveryPageRow, a *integrationTurnPlan) (deliveryRowProof, error) {
	var zero deliveryRowProof
	sc := r.state.scope
	if a == nil || a.control != nil || !v.Packet.Valid || v.Packet.String != v.Result || len(v.Events) != 0 || !reflect.DeepEqual(v.Audience, []string{r.state.record.Player}) {
		return zero, ErrDenied
	}
	valid := v.State == "PENDING" && v.Revision == 2 || (v.State == "READY" || v.State == "FAILED") && v.Revision == 3 || v.State == "WITHDRAWN" && (v.Revision == 3 || v.Revision == 4)
	media := turnIdentity(sc.TenantID, sc.ProductID, sc.WorldInstanceID, v.Result, "AUDIO")
	if !valid || v.ID != turnIdentity(sc.WorldInstanceID, v.Result, v.Result, media, strconv.FormatUint(v.Revision, 10), v.State) {
		return zero, ErrDenied
	}
	var original deliveryPageRow
	var ev, au, li []byte
	id := turnIdentity(sc.TenantID, sc.ProductID, sc.WorldInstanceID, "TURN", v.Result, "1")
	e := q.QueryRowContext(r.ctx, `SELECT update_seq,business_update_id,kind,result_ref,task_ref,event_refs,packet_ref,delivery_state,resource_revision,audience_refs,lineage_refs FROM delivery_updates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND business_update_id=?`, sc.TenantID, sc.ProductID, sc.WorldInstanceID, id).Scan(&original.Seq, &original.ID, &original.Kind, &original.Result, &original.Task, &ev, &original.Packet, &original.State, &original.Revision, &au, &li)
	if e != nil || original.Kind != "TURN" || original.Seq >= v.Seq || original.Task != v.Task || original.Result != v.Result || json.Unmarshal(ev, &original.Events) != nil || json.Unmarshal(au, &original.Audience) != nil || json.Unmarshal(li, &original.Lineage) != nil || !reflect.DeepEqual(original.Lineage, v.Lineage) {
		return zero, ErrDenied
	}
	proof, e := r.rowProof(q, original, a)
	if e != nil {
		return zero, e
	}
	proof.OriginalEvents = proof.Events
	proof.Events = nil
	return proof, nil
}

func (r *IntegrationRead) validateCurrentMedia(v deliveryPageRow, m mediaRecord) error {
	s := r.state.scope
	if m.Generation != r.state.eligible.identity.Generation {
		return ErrDenied
	}
	if e := validateMediaShape(s.ProductScope, s.WorldInstanceID, v.Result, v.Lineage, m); e != nil {
		return e
	}
	valid := m.State == "PENDING" && m.Revision == 2 || (m.State == "READY" || m.State == "FAILED") && m.Revision == 3 || m.State == "WITHDRAWN" && (m.Revision == 3 || m.Revision == 4)
	if !valid || m.Revision < v.Revision || m.Revision == v.Revision && m.State != v.State || v.State == "READY" && m.State != "READY" && m.State != "WITHDRAWN" || v.State == "FAILED" && m.State != "FAILED" && m.State != "WITHDRAWN" || v.State == "WITHDRAWN" && m.State != "WITHDRAWN" {
		return ErrDenied
	}
	return nil
}

func (r *IntegrationRead) packetReference(v deliveryPageRow) DeliveryPacketReference {
	out := DeliveryPacketReference{DeliveryID: v.ID, TaskID: v.Task, PacketID: v.Packet.String, Revision: v.Revision, EventIDs: append([]string(nil), v.Events...), Kind: v.Kind, MediaState: "NONE", Available: true}
	if v.Kind == "MEDIA" {
		m := r.page.Media[v.ID]
		out.MediaState = v.State
		out.MediaID = m.Media
		out.AssetID = m.Asset
		out.Available = v.State != "READY" || m.State == "READY"
	}
	return out
}

// Shape is data validation, not current use, body or cleanup authority. Actual
// SQL NULL bits are retained independently of the decoded zero timestamp.
func mediaSlots(m mediaRecord) []string {
	var out []string
	for n := uint64(1); n <= m.Attempts; n++ {
		out = append(out, turnIdentity(m.Asset, "runtime-media", strconv.FormatUint(n, 10)))
	}
	return out
}
func mediaPurgeIntent(scope domain.ProductScope, m mediaRecord) string {
	v := []string{"runtime-media-purge-intent.v1", scope.TenantID, scope.ProductID, m.World, m.Packet, m.Media, m.Asset, m.Result, m.Accepted.UTC().Format(time.RFC3339Nano), m.Deadline.UTC().Format(time.RFC3339Nano), strconv.FormatUint(m.Fence, 10), strconv.FormatUint(m.Attempts, 10), strconv.FormatUint(m.Generation, 10)}
	return turnIdentity(append(v, mediaSlots(m)...)...)
}
func validateMediaShape(scope domain.ProductScope, world, result string, lineage []string, m mediaRecord) error {
	var sources []string
	if m.Asset != turnIdentity(scope.TenantID, scope.ProductID, world, result, "AUDIO", "asset") || m.Media != turnIdentity(scope.TenantID, scope.ProductID, world, result, "AUDIO") || m.World != world || m.Packet != result || m.Result != result || json.Unmarshal(m.Sources, &sources) != nil || !reflect.DeepEqual(sources, lineage) || m.Accepted.IsZero() || m.Deadline.Sub(m.Accepted) != 60*time.Second || m.Attempts < 1 || m.Attempts > 2 || m.Fence != m.Attempts || !m.OwnerValid || m.Generation == 0 {
		return ErrDenied
	}
	terminal := m.State == "FAILED" || m.State == "WITHDRAWN"
	valid := m.State == "PENDING" && m.Revision == 2 || (m.State == "READY" || m.State == "FAILED") && m.Revision == 3 || m.State == "WITHDRAWN" && (m.Revision == 3 || m.Revision == 4)
	if !valid {
		return ErrDenied
	}
	producer := m.LeaseOwner == turnIdentity(m.Asset, "owner", strconv.FormatUint(m.Fence, 10)) && m.LeaseValid && m.Lease.After(m.Accepted) && !m.Lease.After(m.Deadline)
	intent := terminal && m.LeaseOwner == mediaPurgeIntent(scope, m) && !m.LeaseValid
	if !producer && !intent {
		return ErrDenied
	}
	if m.Storage != "" {
		found := false
		for _, slot := range mediaSlots(m) {
			found = found || slot == m.Storage
		}
		if !found || m.Bytes != 160044 || m.MIME != "audio/wav" || len(m.Hash) != 64 {
			return ErrDenied
		}
		if _, e := hex.DecodeString(m.Hash); e != nil {
			return ErrDenied
		}
	} else if m.Bytes != 0 || m.Hash != "" || m.MIME != "" || m.State == "READY" {
		return ErrDenied
	}
	return nil
}

// retirementProof has a closed structural purpose. It is never placed in the
// caller context and cannot be passed to approved-body or provider operations.
type mediaRetirementProof struct {
	source           *mediaSource
	record           mediaRecord
	installation     string
	generation       uint64
	creation         sessionCreationProof
	publications     []withdrawalPublication
	livePublications []bool
	witness          []string
	witnessRaw       [][]byte
	keys             []LockKey
	gates            []byte
}

func (s *Store) mediaRetained(ctx context.Context, q withdrawalQuerier, asset string) (ret *mediaRetirementProof, err error) {
	stage := "locator"
	defer func() {
		if err != nil {
			err = fmt.Errorf("retained %s: %w", stage, err)
		}
	}()
	if domain.ValidateID(asset) != nil {
		return nil, ErrDenied
	}
	var tenant, product, task, world, player, service, customer, serviceSubject, session, exchange, key, quote, actor string
	var raw, hash []byte
	var use integrationUse
	e := q.QueryRowContext(ctx, `SELECT m.tenant_id,m.product_id,h.task_id,h.world_instance_id,h.owner_subject_id,i.issuer_service_key,i.customer_id,k.subject_id,i.principal_ref,x.original_key,h.original_key,h.quote_id,h.actor_id,h.command_hash,c.document,i.accepted_use_generation,i.accepted_consent_revision,i.accepted_use_expires_at FROM delivery_media m JOIN harness_turn_commits c ON c.tenant_id=m.tenant_id AND c.product_id=m.product_id AND c.result_id=m.original_result_ref JOIN harness_turns h ON h.tenant_id=c.tenant_id AND h.product_id=c.product_id AND h.task_id=c.task_id JOIN integration_task_principals i ON i.tenant_id=h.tenant_id AND i.product_id=h.product_id AND i.task_id=h.task_id JOIN integration_principals k ON k.principal_id=i.issuer_service_key AND k.kind='SERVICE' AND k.tenant_id=i.tenant_id AND k.product_id=i.product_id AND k.customer_id=i.customer_id JOIN integration_principals z ON z.principal_id=i.principal_ref AND z.kind='CLIENT' JOIN integration_receipts x ON x.receipt_ref=z.issuance_receipt AND x.tenant_id=z.tenant_id AND x.product_id=z.product_id AND x.owner_id=z.issuer_service_key AND x.operation='EXCHANGE_SESSION' WHERE m.asset_id=?`, asset).Scan(&tenant, &product, &task, &world, &player, &service, &customer, &serviceSubject, &session, &exchange, &key, &quote, &actor, &hash, &raw, &use.Generation, &use.Revision, &use.Expires)
	if e != nil {
		return nil, fmt.Errorf("retained original lookup: %w", e)
	}
	if len(hash) != 32 {
		return nil, ErrDenied
	}
	stage = "core identity"
	scope := domain.Scope{ProductScope: domain.ProductScope{TenantID: tenant, ProductID: product, SubjectID: player}, WorldInstanceID: world}
	var core domain.CandidateCommit
	if scope.Validate() != nil || json.Unmarshal(raw, &core) != nil || core.Metadata.Identity.Scope != scope || core.Metadata.Identity.TaskID != task || core.ResultID != turnIdentity(task, "result") || core.ReceiptKey != turnIdentity(task, "world-commit") || task != turnIdentity(tenant, product, player, world, "TURN", key) {
		return nil, ErrDenied
	}
	principal := domain.IntegrationPrincipal{Kind: domain.IntegrationService, PrincipalID: service, Tenant: tenant, Product: product, Customer: customer, StableSubject: serviceSubject}
	stage = "session issuance"
	issued, e := sessionRead(ctx, q, principal, exchange)
	if e != nil || issued.ID != session || issued.Player != player || issued.World != world {
		return nil, ErrDenied
	}
	a := &integrationTurnPlan{store: s, service: currentIntegration{store: s, principal: principal}, session: issued, scope: scope, task: task, key: key, quote: quote, action: "TURN", use: use}
	copy(a.hash[:], hash)
	a.eligibility.entitlement.ID = issued.Entitlement
	a.eligibility.identity.Installation = issued.Installation
	if e = a.verifyIntake(ctx, q, a.intakeRequest(), task, true, true); e != nil {
		return nil, fmt.Errorf("retained intake: %w", e)
	}
	stage = "creation"
	created, e := sessionCreation(ctx, q, principal, world, player, core.Versions.ReleaseID)
	if e != nil {
		return nil, e
	}
	stage = "original commit"
	receipt, e := queryReceipt(ctx, q, core.ReceiptKey)
	_, expectedHash, he := harnessPayload(core.Metadata)
	want := BarrierRequest{ProductScope: scope.ProductScope, WorldScope: &scope, OwnerSubjectID: player, TaskID: task, Operation: "HARNESS_TURN_COMMIT", CommandKey: core.ReceiptKey, CommitKey: core.ReceiptKey, PayloadHash: expectedHash}
	if e != nil || he != nil || !receipt.matches(want) {
		return nil, ErrDenied
	}
	stage = "committed result"
	var committed bool
	if e = q.QueryRowContext(ctx, "SELECT t.state='COMMITTED' AND a.status='COMMITTED' AND a.owner_subject_id=? AND a.task_id=? AND a.revision=? FROM tasks t JOIN approved_results a ON a.tenant_id=t.tenant_id AND a.product_id=t.product_id AND a.record_id=? WHERE t.tenant_id=? AND t.product_id=? AND t.task_id=?", player, task, core.Versions.WorldRevision, core.ResultID, tenant, product, task).Scan(&committed); e != nil || !committed {
		return nil, ErrDenied
	}
	p := &mediaSource{scope: scope, task: task, actor: actor, turn: a, core: core, asset: turnIdentity(tenant, product, world, core.ResultID, "AUDIO", "asset"), media: turnIdentity(tenant, product, world, core.ResultID, "AUDIO")}
	if p.asset != asset {
		return nil, ErrDenied
	}
	stage = "installation"
	out := &mediaRetirementProof{source: p, creation: created}
	p.retirement = out
	if e = q.QueryRowContext(ctx, `SELECT CAST(JSON_ARRAY(t.permission_version,t.enabled,p.permission_version,p.enabled,g.permission_version,g.privacy_epoch,g.view_epoch,g.enabled,g.can_execute,g.can_control,g.can_govern,g.can_migrate) AS CHAR) FROM tenant_gates t JOIN product_gates p USING(tenant_id) JOIN subject_governance g USING(tenant_id,product_id) WHERE t.tenant_id=? AND p.product_id=? AND g.subject_id=?`, tenant, product, player).Scan(&out.gates); e != nil {
		return nil, e
	}
	if e = q.QueryRowContext(ctx, "SELECT installation_id,generation FROM memory_installation WHERE singleton=1").Scan(&out.installation, &out.generation); e != nil || out.installation != issued.Installation {
		return nil, ErrDenied
	}
	stage = "media shape"
	out.record, e = readMedia(ctx, q, p)
	if e != nil || out.record.Generation != out.generation || validateMediaShape(scope.ProductScope, world, core.ResultID, []string{core.Versions.ReleaseID}, out.record) != nil {
		return nil, ErrDenied
	}
	out.keys = sessionKeys(principal, issued, []LockKey{{Rank: 90, Kind: "integration_consent", IDs: []string{tenant, product, player}}, {Rank: 90, Kind: "delivery_media", IDs: []string{tenant, product, asset}}, harnessTaskKey(scope.ProductScope, task), harnessTaskKey(scope.ProductScope, quote), {Rank: 110, Kind: "commit_receipts", IDs: []string{world}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{turnIdentity(task, "accepted")}}, {Rank: 110, Kind: "commit_receipts", IDs: []string{core.ReceiptKey}}, {Rank: 110, Kind: "approved_results", IDs: []string{tenant, product, core.ResultID}}})
	// Preserve original event identity/COMMIT and accepted source graph. No event
	// payload is returned or treated as a historical cryptographic body hash.
	stage = "events"
	for _, id := range core.EventIDs {
		var b []byte
		var ok bool
		e = q.QueryRowContext(ctx, "SELECT r.document,(e.status='COMMITTED' AND e.owner_subject_id=? AND e.task_id=? AND e.world_instance_id=? AND e.revision=?) FROM events e JOIN runtime_events r USING(tenant_id,product_id,record_id) WHERE e.tenant_id=? AND e.product_id=? AND e.record_id=?", player, task, world, core.Versions.WorldRevision, tenant, product, id).Scan(&b, &ok)
		var kind struct{ Kind string }
		if e != nil || !ok || json.Unmarshal(b, &kind) != nil || kind.Kind != "ACTION_BUNDLE" {
			return nil, ErrDenied
		}
		out.witnessRaw = append(out.witnessRaw, b)
		out.keys = append(out.keys, LockKey{Rank: 110, Kind: "events", IDs: []string{tenant, product, id}})
	}
	stage = "publication graph"
	queue := []string{core.Versions.ReleaseID}
	seen := map[string]bool{}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		if len(seen) > 200 {
			return nil, ErrDenied
		}
		var doc []byte
		if e = q.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", tenant, product, id).Scan(&doc); e != nil {
			return nil, e
		}
		var release generated.Release
		if generated.Decode("Release", doc, &release) != nil {
			return nil, ErrDenied
		}
		ps := domain.ProductScope{TenantID: tenant, ProductID: product, SubjectID: string(release.PublishedBySubjectID)}
		pub, live, e := readWithdrawalPublication(ctx, q, ps, id)
		if e != nil {
			return nil, e
		}
		out.publications = append(out.publications, pub)
		out.livePublications = append(out.livePublications, live)
		out.keys = append(out.keys, LockKey{Rank: 90, Kind: "content_release", IDs: []string{tenant, product, id}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{tenant, product, pub.result}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{tenant, product, string(release.CheckReportID)}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{pub.result}})
		for _, dep := range release.DependencyLocks {
			var actual []byte
			if e = q.QueryRowContext(ctx, "SELECT content_hash FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", tenant, product, dep.ReleaseID).Scan(&actual); e != nil || hex.EncodeToString(actual) != dep.ContentHash {
				return nil, ErrDenied
			}
			queue = append(queue, string(dep.ReleaseID))
		}
		if !live {
			rows, e := q.QueryContext(ctx, "SELECT commit_key FROM commit_receipts WHERE tenant_id=? AND product_id=? AND owner_subject_id=? AND operation=? AND phase='COMMIT'", tenant, product, ps.SubjectID, withdrawalOperation)
			if e != nil {
				return nil, e
			}
			var keys []string
			for rows.Next() {
				var k string
				if e = rows.Scan(&k); e != nil {
					break
				}
				keys = append(keys, k)
			}
			re := rows.Err()
			rows.Close()
			if e != nil || re != nil {
				return nil, ErrDenied
			}
			h, he := withdrawalHash(struct {
				Scope             domain.ProductScope
				Operation, Target string
				Input             any
			}{ps, withdrawalOperation, id, struct{ ExpectedContentHash, Action string }{release.ContentHash, "WITHDRAW"}})
			if he != nil {
				return nil, he
			}
			matches := 0
			for _, k := range keys {
				rr, e := queryReceipt(ctx, q, k)
				if e != nil {
					return nil, e
				}
				req := BarrierRequest{ProductScope: ps, OwnerSubjectID: ps.SubjectID, Operation: withdrawalOperation, CommandKey: rr.command, CommitKey: k, PayloadHash: h, commandOnly: true}
				if rr.matches(req) && k == creationID(tenant, product, ps.SubjectID, withdrawalOperation, rr.command) {
					matches++
					out.witness = append(out.witness, "PUBLICATION:"+id+":"+k)
					out.keys = append(out.keys, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{k}})
				}
			}
			if matches != 1 {
				return nil, ErrDenied
			}
		}
	}
	// Current REVOKE is an explicit fact and original committed receipt, not an
	// arbitrary failed current read. All prior generation/hash facts are checked.
	stage = "current consent"
	var revision, generation uint64
	var action, policy, ref string
	var accepted, expiry sql.NullTime
	e = q.QueryRowContext(ctx, "SELECT revision,action,policy,receipt_ref,use_authority_generation,accepted_at,expires_at FROM integration_self_consent_current WHERE tenant_id=? AND product_id=? AND player_subject_id=?", tenant, product, player).Scan(&revision, &action, &policy, &ref, &generation, &accepted, &expiry)
	if e != nil || policy != playerConsentPolicy {
		return nil, ErrDenied
	}
	rows, e := q.QueryContext(ctx, "SELECT original_key,principal_ref FROM integration_self_consent_facts WHERE tenant_id=? AND product_id=? AND player_subject_id=? AND revision<=? ORDER BY revision", tenant, product, player, revision)
	if e != nil {
		return nil, e
	}
	for rows.Next() {
		var key, id string
		if e = rows.Scan(&key, &id); e != nil {
			break
		}
		out.keys = append(out.keys, LockKey{Rank: 15, Kind: "integration_principal", IDs: []string{id}}, LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{tenant, product, player, key}})
	}
	re := rows.Err()
	rows.Close()
	if e != nil || re != nil {
		return nil, ErrDenied
	}
	stage = "revoke witness"
	if action == "REVOKE" {
		var key string
		var h, b []byte
		var matching bool
		e = q.QueryRowContext(ctx, `SELECT f.original_key,f.payload_hash,x.result_metadata,(f.revision=? AND f.action='REVOKE' AND f.policy=? AND f.receipt_ref=? AND f.use_authority_generation=? AND f.accepted_at<=>? AND f.expires_at IS NULL AND x.committed_at=f.recorded_at) FROM integration_self_consent_facts f JOIN integration_receipts x ON x.tenant_id=f.tenant_id AND x.product_id=f.product_id AND x.owner_id=f.player_subject_id AND x.original_key=f.original_key AND x.receipt_ref=f.receipt_ref AND x.operation='PLAYER_SELF_CONSENT' WHERE f.tenant_id=? AND f.product_id=? AND f.player_subject_id=? AND f.revision=?`, revision, policy, ref, generation, accepted, tenant, product, player, revision).Scan(&key, &h, &b, &matching)
		var receipt generated.PlayerSelfConsentReceipt
		var hh [32]byte
		copy(hh[:], h)
		if e != nil || !matching || expiry.Valid || generation <= use.Generation || len(h) != 32 || json.Unmarshal(b, &receipt) != nil || validatePlayerConsentReceipt(ctx, q, issued, key, hh, receipt) != nil {
			return nil, ErrDenied
		}
		var disabled bool
		if e = q.QueryRowContext(ctx, "SELECT enabled=0 FROM memory_subject_permissions WHERE tenant_id=? AND product_id=? AND caller_subject_id=? AND data_subject_id=? AND action='memory.generation.use' AND allowed_use='PRIVATE_RESPONSE'", tenant, product, player, player).Scan(&disabled); e != nil || !disabled {
			return nil, ErrDenied
		}
		out.witness = append(out.witness, "REVOKE:"+ref)
		out.witnessRaw = append(out.witnessRaw, b)
	}
	stamp, _ := json.Marshal([]any{revision, generation, action, policy, ref, accepted, expiry})
	out.witnessRaw = append(out.witnessRaw, stamp)
	stage = "media history"
	if e = s.mediaRetainedHistory(ctx, q, p, out.record); e != nil {
		return nil, e
	}
	for rev := uint64(2); rev <= 4; rev++ {
		for _, state := range []string{"PENDING", "READY", "FAILED", "WITHDRAWN"} {
			out.keys = append(out.keys, LockKey{Rank: 110, Kind: "delivery_update", IDs: []string{tenant, product, mediaUpdateID(p, rev, state)}})
		}
	}
	out.keys = append(out.keys, LockKey{Rank: 110, Kind: "delivery_update", IDs: []string{tenant, product, turnIdentity(tenant, product, world, "TURN", core.ResultID, "1")}})
	return out, nil
}

// The complete persisted transition chain is structural evidence. It never
// substitutes for current playback permission or invents a historical file hash.
func (s *Store) mediaRetainedHistory(ctx context.Context, q withdrawalQuerier, p *mediaSource, m mediaRecord) error {
	rows, e := q.QueryContext(ctx, `SELECT update_seq,business_update_id,kind,result_ref,task_ref,event_refs,packet_ref,delivery_state,resource_revision,audience_refs,lineage_refs FROM delivery_updates WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND result_ref=? AND kind IN ('TURN','MEDIA') ORDER BY update_seq`, p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, p.core.ResultID)
	if e != nil {
		return e
	}
	defer rows.Close()
	var chain []deliveryPageRow
	for rows.Next() {
		var v deliveryPageRow
		var ev, au, li []byte
		if e = rows.Scan(&v.Seq, &v.ID, &v.Kind, &v.Result, &v.Task, &ev, &v.Packet, &v.State, &v.Revision, &au, &li); e != nil {
			return e
		}
		if json.Unmarshal(ev, &v.Events) != nil || json.Unmarshal(au, &v.Audience) != nil || json.Unmarshal(li, &v.Lineage) != nil || v.Task != p.task || !v.Packet.Valid || v.Packet.String != p.core.ResultID || !reflect.DeepEqual(v.Audience, []string{p.scope.SubjectID}) || !reflect.DeepEqual(v.Lineage, []string{p.core.Versions.ReleaseID}) {
			return ErrDenied
		}
		chain = append(chain, v)
		if len(chain) > 4 {
			return ErrDenied
		}
	}
	if e = rows.Err(); e != nil {
		return e
	}
	if len(chain) < 2 {
		return ErrDenied
	}
	first := chain[0]
	if first.Kind != "TURN" || first.ID != turnIdentity(p.scope.TenantID, p.scope.ProductID, p.scope.WorldInstanceID, "TURN", p.core.ResultID, "1") || first.Revision != 1 || first.State != "COMMITTED" || !reflect.DeepEqual(first.Events, p.core.EventIDs) {
		return ErrDenied
	}
	previous := ""
	rev := uint64(1)
	for i, v := range chain[1:] {
		if v.Seq <= chain[i].Seq || v.Kind != "MEDIA" || v.Revision != rev+1 || len(v.Events) != 0 || v.ID != mediaUpdateID(p, v.Revision, v.State) {
			return ErrDenied
		}
		valid := i == 0 && v.State == "PENDING" || previous == "PENDING" && (v.State == "READY" || v.State == "FAILED" || v.State == "WITHDRAWN") || (previous == "READY" || previous == "FAILED") && v.State == "WITHDRAWN"
		if !valid {
			return ErrDenied
		}
		previous = v.State
		rev = v.Revision
	}
	if previous != m.State || rev != m.Revision {
		return ErrDenied
	}
	ready := false
	for _, v := range chain {
		ready = ready || v.State == "READY"
	}
	if ready != (m.Storage != "") {
		return ErrDenied
	}
	return nil
}

func (s *Store) planMediaRetirement(ctx context.Context, p *mediaRetirementProof) error {
	sc := p.source.scope
	var v domain.Versions
	var status string
	e := s.DB.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", sc.TenantID, sc.ProductID, sc.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status)
	if e != nil {
		return e
	}
	// This private request only selects sorted locks. It is never dispatched to
	// the execution barrier and grants neither a caller principal nor body use.
	key := turnIdentity(p.source.asset, "retained-media-maintenance")
	p.source.request = BarrierRequest{ProductScope: sc.ProductScope, WorldScope: &p.source.scope, Versions: &v, ExpectedWorldStatus: status, OwnerSubjectID: sc.SubjectID, TaskID: p.source.task, Operation: "RUNTIME_MEDIA_RETAINED", CommandKey: key, CommitKey: key, commandOnly: true, approvedHistoryRead: true, Locks: append([]LockKey(nil), p.keys...)}
	return nil
}
func sameMediaRetirement(a, b *mediaRetirementProof) bool {
	return a != nil && b != nil && a.source.scope == b.source.scope && a.source.task == b.source.task && a.source.actor == b.source.actor && reflect.DeepEqual(a.source.core, b.source.core) && reflect.DeepEqual(a.source.turn.session, b.source.turn.session) && a.source.turn.hash == b.source.turn.hash && a.source.turn.use == b.source.turn.use && a.installation == b.installation && a.generation == b.generation && reflect.DeepEqual(a.creation, b.creation) && reflect.DeepEqual(a.publications, b.publications) && reflect.DeepEqual(a.livePublications, b.livePublications) && reflect.DeepEqual(a.witness, b.witness) && reflect.DeepEqual(a.witnessRaw, b.witnessRaw) && reflect.DeepEqual(a.keys, b.keys) && reflect.DeepEqual(a.gates, b.gates)
}

// Structural maintenance locks the same rows in the original total order but
// does not demand a revoked player's execution right or a live publication.
// Only the private plan produced above reaches this branch; no generic barrier
// or caller-supplied maintenance purpose is added.
func (s *Store) mediaRetirementLock(ctx context.Context, tx *sql.Tx, p *mediaSource, k LockKey) (bool, error) {
	if p.retirement == nil || p.retirement.source != p || p.turn == nil || p.turn.store != s {
		return true, ErrDenied
	}
	t, prod, sub := p.scope.TenantID, p.scope.ProductID, p.scope.SubjectID
	var query string
	var args []any
	switch {
	case k.Rank == 10:
		query = "SELECT tenant_id FROM tenant_gates WHERE tenant_id=? FOR UPDATE"
		args = []any{t}
	case k.Rank == 20:
		if e := studioLockRows(ctx, tx, "SELECT installation_id FROM memory_installation WHERE installation_id=? AND generation=? FOR UPDATE", p.retirement.installation, p.retirement.generation); e != nil {
			return true, e
		}
		query = "SELECT product_id FROM product_gates WHERE tenant_id=? AND product_id=? FOR UPDATE"
		args = []any{t, prod}
	case k.Rank == 30:
		query = "SELECT subject_id FROM subject_governance WHERE tenant_id=? AND product_id=? AND subject_id=? FOR UPDATE"
		args = []any{t, prod, sub}
	case k.Rank == 40:
		query = "SELECT world_instance_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? FOR UPDATE"
		args = []any{t, prod, p.scope.WorldInstanceID}
	case k.Rank == 90 && k.Kind == "content_release":
		query = "SELECT release_id FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=? FOR UPDATE"
		args = []any{k.IDs[0], k.IDs[1], k.IDs[2]}
		if e := marker(ctx, tx, k); e != nil {
			return true, e
		}
		if e := studioLockRows(ctx, tx, query, args...); e != nil {
			return true, e
		}
		return true, studioLockRows(ctx, tx, "SELECT release_id FROM releases WHERE tenant_id=? AND product_id=? AND release_id=? FOR UPDATE", args...)
	case k.Rank == 90 && k.Kind == "content_check":
		query = "SELECT result_id FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? FOR UPDATE"
		args = []any{k.IDs[0], k.IDs[1], k.IDs[2]}
		if e := marker(ctx, tx, k); e != nil {
			return true, e
		}
		if e := studioLockRows(ctx, tx, query, args...); e != nil {
			return true, e
		}
		return true, studioLockRows(ctx, tx, "SELECT report_id FROM content_checks WHERE tenant_id=? AND product_id=? AND report_id=? FOR UPDATE", args...)
	default:
		return false, nil
	}
	if e := marker(ctx, tx, k); e != nil {
		return true, e
	}
	return true, studioLockRows(ctx, tx, query, args...)
}
func (s *Store) mediaRetirementCurrent(ctx context.Context, u *unit, p *mediaRetirementProof, expected mediaRecord) error {
	if e := s.checkDeliveryAdmission(ctx); e != nil {
		return e
	}
	n, e := s.mediaRetained(ctx, u.tx, p.source.asset)
	if e != nil {
		return e
	}
	if !sameMediaRetirement(n, p) || !reflect.DeepEqual(n.record, expected) {
		return ErrReplan
	}
	var v domain.Versions
	var status string
	e = u.tx.QueryRowContext(ctx, "SELECT release_id,world_revision,control_version,privacy_epoch,view_epoch,status FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=?", p.source.scope.TenantID, p.source.scope.ProductID, p.source.scope.WorldInstanceID).Scan(&v.ReleaseID, &v.WorldRevision, &v.ControlVersion, &v.PrivacyEpoch, &v.ViewEpoch, &status)
	if e != nil || v != *p.source.request.Versions || status != p.source.request.ExpectedWorldStatus {
		return ErrReplan
	}
	return ctx.Err()
}
func (s *Store) ReconcileSyntheticMedia(ctx context.Context, owner DeliveryReaderOwner, asset string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, e := s.mediaOwner(ctx, owner); e != nil {
		return false, e
	}
	p, e := s.mediaRetained(ctx, s.DB, asset)
	if e != nil {
		return false, e
	}
	if e = s.planMediaRetirement(ctx, p); e != nil {
		return false, e
	}
	u, done, e := s.mediaTx(ctx, p.source)
	if e != nil {
		return false, e
	}
	defer done()
	if e = s.mediaRetirementCurrent(ctx, u, p, p.record); e != nil {
		return false, e
	}
	m := p.record
	if m.State == "WITHDRAWN" {
		return false, nil
	}
	if len(p.witness) == 0 {
		var expired bool
		// Only authoritative DB time closes the original acceptance.
		if e = u.tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)>=execute_by FROM delivery_media WHERE tenant_id=? AND product_id=? AND asset_id=?", p.source.scope.TenantID, p.source.scope.ProductID, asset).Scan(&expired); e != nil {
			return false, e
		}
		if m.State != "PENDING" || !expired {
			return false, nil
		}
		m.State = "FAILED"
	} else {
		m.State = "WITHDRAWN"
	}
	m.Revision++
	result, e := u.tx.ExecContext(ctx, "UPDATE delivery_media SET state=?,revision=? WHERE tenant_id=? AND product_id=? AND asset_id=? AND state=? AND revision=? AND fence=? AND lease_owner=?", m.State, m.Revision, p.source.scope.TenantID, p.source.scope.ProductID, asset, p.record.State, p.record.Revision, m.Fence, m.LeaseOwner)
	if e != nil {
		return false, e
	}
	n, e := result.RowsAffected()
	if e != nil || n != 1 {
		return false, ErrReplan
	}
	if e = s.appendMediaUpdate(ctx, u, p.source, m); e != nil {
		return false, e
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, u.tx); e != nil {
			return false, e
		}
	}
	if e = s.mediaRetirementCurrent(ctx, u, p, m); e != nil {
		return false, e
	}
	w := p.source.write
	var h uint64
	if w == nil || w.tx != u.tx || w.unit != u || w.source != p.source || !w.appended {
		return false, ErrDenied
	}
	if e = u.tx.QueryRowContext(ctx, "SELECT update_seq FROM delivery_streams WHERE tenant_id=? AND product_id=? AND world_instance_id=?", p.source.scope.TenantID, p.source.scope.ProductID, p.source.scope.WorldInstanceID).Scan(&h); e != nil || h != w.h {
		return false, ErrReplan
	}
	commit := s.Commit
	if commit == nil {
		commit = func(_ context.Context, tx *sql.Tx) error { return tx.Commit() }
	}
	if e = commit(ctx, u.tx); e != nil {
		return false, ErrCommitUnknown
	}
	return true, nil
}

type RuntimeMediaPurgePlan struct {
	Scope domain.ProductScope
	Asset string
	Slots []string
}
type RuntimeMediaPurgeClaim struct {
	self     *RuntimeMediaPurgeClaim
	store    *Store
	owner    DeliveryReaderOwner
	files    RuntimeMediaFileOwner
	expected *mediaRetirementProof
	mu       sync.Mutex
	closed   bool
}

func (c *RuntimeMediaPurgeClaim) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.self != c {
		return ErrDenied
	}
	c.closed = true
	return nil
}
func (s *Store) ClaimRuntimeMediaPurge(ctx context.Context, owner DeliveryReaderOwner, f RuntimeMediaFileOwner, asset string) (*RuntimeMediaPurgeClaim, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	actual, e := s.mediaOwner(ctx, owner)
	if e != nil || f != actual {
		return nil, ErrDenied
	}
	p, e := s.mediaRetained(ctx, s.DB, asset)
	if e != nil {
		return nil, e
	}
	if p.record.State != "FAILED" && p.record.State != "WITHDRAWN" {
		return nil, ErrDenied
	}
	if e = s.planMediaRetirement(ctx, p); e != nil {
		return nil, e
	}
	u, done, e := s.mediaTx(ctx, p.source)
	if e != nil {
		return nil, e
	}
	defer done()
	if e = s.mediaRetirementCurrent(ctx, u, p, p.record); e != nil {
		return nil, e
	}
	m := p.record
	intent := mediaPurgeIntent(p.source.scope.ProductScope, m)
	if m.LeaseOwner != intent {
		result, e := u.tx.ExecContext(ctx, "UPDATE delivery_media SET lease_owner=?,lease_expires_at=NULL WHERE tenant_id=? AND product_id=? AND asset_id=? AND state=? AND revision=? AND lease_owner=? AND fence=?", intent, p.source.scope.TenantID, p.source.scope.ProductID, asset, m.State, m.Revision, m.LeaseOwner, m.Fence)
		if e != nil {
			return nil, e
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return nil, ErrReplan
		}
		m.LeaseOwner = intent
		m.Lease = time.Time{}
		m.LeaseValid = false
		if s.BeforeCommit != nil {
			if e = s.BeforeCommit(ctx, u.tx); e != nil {
				return nil, e
			}
		}
		if e = s.mediaRetirementCurrent(ctx, u, p, m); e != nil {
			return nil, e
		}
		commit := s.Commit
		if commit == nil {
			commit = func(_ context.Context, tx *sql.Tx) error { return tx.Commit() }
		}
		// No claim escapes an uncertain intent COMMIT. A later owning call may
		// inspect the original permanent identity; it cannot mint a new intent key.
		if e = commit(ctx, u.tx); e != nil {
			return nil, ErrCommitUnknown
		}
	}
	p.record = m
	c := &RuntimeMediaPurgeClaim{store: s, owner: owner, files: f, expected: p}
	c.self = c
	return c, nil
}
func (s *Store) RecheckRuntimeMediaPurge(ctx context.Context, f RuntimeMediaFileOwner, c *RuntimeMediaPurgeClaim) (RuntimeMediaPurgePlan, error) {
	var zero RuntimeMediaPurgePlan
	if c == nil {
		return zero, ErrDenied
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.self != c || c.closed || c.store != s || c.files != f {
		return zero, ErrDenied
	}
	if actual, e := s.mediaOwner(ctx, c.owner); e != nil || actual != f {
		return zero, ErrDenied
	}
	p := c.expected
	if p.record.LeaseValid || p.record.LeaseOwner != mediaPurgeIntent(p.source.scope.ProductScope, p.record) {
		return zero, ErrDenied
	}
	u, done, e := s.mediaTx(ctx, p.source)
	if e != nil {
		return zero, e
	}
	defer done()
	if e = s.mediaRetirementCurrent(ctx, u, p, p.record); e != nil {
		return zero, e
	}
	return RuntimeMediaPurgePlan{Scope: p.source.scope.ProductScope, Asset: p.source.asset, Slots: mediaSlots(p.record)}, nil
}
