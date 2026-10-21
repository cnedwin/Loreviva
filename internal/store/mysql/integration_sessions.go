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
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/localstate"
	"reflect"
	"sort"
	"strconv"
	"time"
)

// Only app assembly binds the independent local root, once per Store. No
// request field, metadata getter or context value can select a key location.
func (s *Store) BindIntegrationSessionKeys(ctx context.Context, root *localstate.Root) error {
	s.integrationOwnerMu.Lock()
	defer s.integrationOwnerMu.Unlock()
	if root == nil || s.integrationSessionRoot != nil || s.integrationClosed.Load() || s.Guard == nil || s.ConfigGuard == nil {
		return ErrDenied
	}
	_, _, release, e := s.EnterConfiguration(ctx)
	if e != nil {
		return e
	}
	defer release()
	s.integrationSessionRoot = root
	return nil
}

type sessionRecord struct {
	ID, Installation, Tenant, Product, Customer, Service, ServiceSubject, Player, World, External, Entitlement, Profile, Key, Hash, KeyRef, Receipt string
	Actions                                                                                                                                         []string
	Nonce                                                                                                                                           []byte
	KeyVersion, EntitlementRevision, MappingRevision, CustomerRevision                                                                              uint64
	Issued, Expires                                                                                                                                 time.Time
}
type sessionEligibility struct {
	identity    creationIdentityState
	mapping     *registeredPlayerSnapshot
	entitlement creationEntitlement
	keys        []LockKey
	creation    sessionCreationProof
}

type sessionCreationProof struct {
	receipt         receiptRow
	initial, result []byte
}

// A Session is attached to a committed creation, not merely an entitlement
// mentioning a world. The historical result is CREATED; the current world is
// deliberately not restricted to that lifecycle state.
func sessionCreation(ctx context.Context, q rowQuerier, p domain.IntegrationPrincipal, world, player, release string) (sessionCreationProof, error) {
	var out sessionCreationProof
	var good bool
	e := q.QueryRowContext(ctx, `SELECT (w.creation_receipt=? AND w.revision=1 AND i.owner_subject_id=? AND i.mode='PRIVATE' AND i.initial_release_id=? AND d.initialization_receipt=? AND d.projection_version=1),i.initialization FROM integration_world_participants w JOIN runtime_instances i USING(tenant_id,product_id,world_instance_id) JOIN delivery_streams d USING(tenant_id,product_id,world_instance_id) JOIN world_controls c USING(tenant_id,product_id,world_instance_id) WHERE w.tenant_id=? AND w.product_id=? AND w.world_instance_id=? AND w.customer_id=? AND w.service_owner_id=? AND w.player_subject_id=? AND w.release_id=? AND w.enabled=1`, world, p.StableSubject, release, world, p.Tenant, p.Product, world, p.Customer, p.StableSubject, player, release).Scan(&good, &out.initial)
	if e != nil || !good {
		return out, ErrDenied
	}
	var init struct {
		ReleaseID, ReleaseHash, CreationCommandID string
		Participants                              []string
		IntegrationInitialLineage                 *IntegrationInitialLineage
	}
	if json.Unmarshal(out.initial, &init) != nil || init.ReleaseID != release || init.CreationCommandID != world || !reflect.DeepEqual(init.Participants, []string{player}) || init.IntegrationInitialLineage == nil {
		return out, ErrDenied
	}
	l := init.IntegrationInitialLineage
	if l.Tenant != p.Tenant || l.Product != p.Product || l.Customer != p.Customer || l.Player != player || l.World != world || l.CreationReceipt != world {
		return out, ErrDenied
	}
	var rootHash []byte
	if e = q.QueryRowContext(ctx, "SELECT content_hash FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", p.Tenant, p.Product, release).Scan(&rootHash); e != nil || len(rootHash) != 32 || hex.EncodeToString(rootHash) != init.ReleaseHash {
		return out, ErrDenied
	}
	out.receipt, e = queryReceipt(ctx, q, world)
	if e != nil || len(out.receipt.hash) != 32 || out.receipt.nullTask.Valid || domain.ValidateIdempotencyKey(out.receipt.command) != nil || creationID(p.Tenant, p.Product, p.StableSubject, "createInstance", out.receipt.command) != world {
		return out, ErrDenied
	}
	var hash [32]byte
	copy(hash[:], out.receipt.hash)
	request := BarrierRequest{ProductScope: domain.ProductScope{TenantID: p.Tenant, ProductID: p.Product, SubjectID: p.StableSubject}, OwnerSubjectID: p.StableSubject, Operation: "createInstance", CommandKey: out.receipt.command, CommitKey: world, PayloadHash: hash, commandOnly: true}
	if !out.receipt.matches(request) {
		return out, ErrDenied
	}
	// ClientCapabilities was not persisted. Preserve the actual original hash;
	// do not invent a payload to reconstruct it. Callers freeze this entire proof
	// before locking and compare it again inside their owning transaction.
	e = q.QueryRowContext(ctx, "SELECT document FROM admin_results WHERE tenant_id=? AND product_id=? AND result_id=? AND owner_subject_id=? AND schema_name='Instance'", p.Tenant, p.Product, world, p.StableSubject).Scan(&out.result)
	a := integrationCreationState{principal: currentIntegration{principal: p}, id: world, request: generated.CreateInstanceRequest{ReleaseID: release}}
	var instance generated.Instance
	if e != nil || generated.Decode("Instance", out.result, &instance) != nil || instance != a.instance() {
		return out, ErrDenied
	}
	return out, nil
}

func (s *Store) sessionEligibility(ctx context.Context, q creationRows, v currentIntegration, input generated.ClientSessionRequest) (sessionEligibility, error) {
	var out sessionEligibility
	var e error
	out.identity, e = s.currentCreationIdentity(ctx, q, v)
	if e != nil {
		return out, e
	}
	p := v.principal
	var player, release, external, entitlement string
	var live bool
	e = q.QueryRowContext(ctx, `SELECT player_subject_id,release_id,external_id,entitlement_id,enabled=1 FROM integration_world_participants WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND customer_id=? AND service_owner_id=?`, p.Tenant, p.Product, input.WorldInstanceID, p.Customer, p.StableSubject).Scan(&player, &release, &external, &entitlement, &live)
	if e != nil || !live || external != input.ExternalSubjectID || entitlement != input.EntitlementReference {
		return out, ErrDenied
	}
	out.creation, e = sessionCreation(ctx, q, p, input.WorldInstanceID, player, release)
	if e != nil {
		return out, e
	}
	rows, e := creationCandidates(ctx, q, p, player, release, input.WorldInstanceID)
	if e != nil {
		return out, e
	}
	found := false
	for _, r := range rows {
		if r.ID == entitlement {
			out.entitlement = r
			found = true
		}
	}
	if !found {
		return out, ErrDenied
	}
	op := domain.OperatorPrincipal{ProductScope: domain.ProductScope{TenantID: p.Tenant, ProductID: p.Product, SubjectID: p.StableSubject}}
	out.mapping, e = readRegisteredPlayer(ctx, q, op, domain.RegisterPlayerAccess{CustomerID: p.Customer, ExternalIDBytes: []byte(external), PlayerSubjectID: player, ExpiresAt: out.entitlement.Expiry, Actions: []string{"READ"}})
	if e != nil || out.mapping == nil {
		return out, ErrDenied
	}
	receipt, owner, key, e := registeredReceipt(ctx, q, op, out.entitlement.Receipt, "REGISTER_PLAYER_ACCESS", player)
	a := integrationCreationState{principal: v, mapping: out.mapping, entitlement: out.entitlement}
	if e != nil || !a.registrationMatches(receipt) {
		return out, ErrDenied
	}
	out.keys = append(out.mapping.receiptKeys(op), LockKey{Rank: 110, Kind: "integration_receipt", IDs: []string{p.Tenant, p.Product, owner, key}})
	out.keys = append(out.keys, LockKey{Rank: 90, Kind: "content_release", IDs: []string{p.Tenant, p.Product, release}}, LockKey{Rank: 90, Kind: "content_check", IDs: []string{p.Tenant, p.Product, input.WorldInstanceID}}, LockKey{Rank: 110, Kind: "commit_receipts", IDs: []string{input.WorldInstanceID}})
	return out, nil
}
func sessionToken(key [32]byte, r sessionRecord) string {
	b, _ := json.Marshal(r)
	m := hmac.New(sha256.New, key[:])
	m.Write([]byte("loreviva.client.session.v1\x00"))
	m.Write(b)
	return "lvcs1_" + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}
func sessionRequest(r sessionRecord) generated.ClientSessionRequest {
	return generated.ClientSessionRequest{ProductID: r.Product, WorldInstanceID: r.World, ExternalSubjectID: r.External, EntitlementReference: r.Entitlement, RequestedActions: append([]string(nil), r.Actions...)}
}
func sessionRead(ctx context.Context, q rowQuerier, p domain.IntegrationPrincipal, key string) (sessionRecord, error) {
	var r sessionRecord
	var b, h []byte
	var ref, state string
	e := q.QueryRowContext(ctx, `SELECT result_metadata,payload_hash,receipt_ref,state FROM integration_receipts WHERE tenant_id=? AND product_id=? AND owner_id=? AND operation='EXCHANGE_SESSION' AND scope_kind='SESSION' AND scope_address=? AND original_key=?`, p.Tenant, p.Product, p.PrincipalID, p.Product, key).Scan(&b, &h, &ref, &state)
	if e != nil {
		return r, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&r) != nil || r.Tenant != p.Tenant || r.Product != p.Product || r.Customer != p.Customer || r.Service != p.PrincipalID || r.ServiceSubject != p.StableSubject || r.Key != key || r.Receipt != ref || state != "COMMITTED" || len(h) != 32 || r.Hash != hex.EncodeToString(h) || len(r.Nonce) != 32 || r.KeyVersion != 1 || !r.Expires.Equal(r.Issued.Add(15*time.Minute)) {
		return r, ErrDenied
	}
	return r, nil
}
func sessionKeys(p domain.IntegrationPrincipal, r sessionRecord, extra []LockKey) []LockKey {
	keys := []LockKey{{Rank: 10, IDs: []string{p.Tenant}}, {Rank: 15, Kind: "integration_principal", IDs: []string{p.PrincipalID}}, {Rank: 15, Kind: "integration_principal", IDs: []string{r.ID}}, {Rank: 20, IDs: []string{p.Tenant, p.Product}}, {Rank: 25, Kind: "integration_entitlement", IDs: []string{p.Tenant, p.Product, p.Customer, r.Entitlement}}, {Rank: 30, IDs: []string{p.Tenant, p.Product, p.StableSubject}}, {Rank: 30, IDs: []string{p.Tenant, p.Product, r.Player}}, {Rank: 35, Kind: "integration_customer", IDs: []string{p.Tenant, p.Product, p.Customer}}, {Rank: 35, Kind: "integration_mapping", IDs: []string{p.Tenant, p.Product, p.Customer, creationExternalHash([]byte(r.External))}}, {Rank: 35, Kind: "integration_participant", IDs: []string{p.Tenant, p.Product, r.World}}, {Rank: 40, IDs: []string{p.Tenant, p.Product, r.World}}, {Rank: 110, Kind: "integration_receipt", IDs: []string{p.Tenant, p.Product, p.PrincipalID, r.Key}}}
	return append(keys, extra...)
}
func (s *Store) currentSessionRow(ctx context.Context, q rowQuerier, r sessionRecord, hash [32]byte) error {
	return s.currentSessionRowForTask(ctx, q, r, hash, false)
}
func (s *Store) currentSessionRowForTask(ctx context.Context, q rowQuerier, r sessionRecord, hash [32]byte, acceptedTask bool) error {
	var live bool
	e := q.QueryRowContext(ctx, `SELECT k.kind='CLIENT' AND k.token_hash=? AND k.revoked=0 AND (? OR k.expires_at>UTC_TIMESTAMP(6)) AND k.tenant_id=? AND k.product_id=? AND k.customer_id=? AND k.subject_id=? AND k.world_instance_id=? AND k.issuer_service_key=? AND k.profile=? AND k.entitlement_id=? AND k.issued_at=? AND k.expires_at=? AND k.nonce=? AND k.key_version=? AND k.key_ref=? AND k.issuance_receipt=? AND h.kind='CLIENT' AND h.principal_id=k.principal_id AND h.installation_id=? FROM integration_principals k JOIN integration_credential_hashes h ON h.token_hash=k.token_hash WHERE k.principal_id=?`, hash[:], acceptedTask, r.Tenant, r.Product, r.Customer, r.Player, r.World, r.Service, r.Profile, r.Entitlement, r.Issued, r.Expires, r.Nonce, r.KeyVersion, r.KeyRef, r.Receipt, r.Installation, r.ID).Scan(&live)
	if e != nil || !live {
		return ErrDenied
	}
	return nil
}

// Key IO precedes SQL under CONFIG. All issuance data, hash reservation and
// original receipt commit together; replay never changes nonce or expiry.
func (s *Store) ExchangeIntegrationSession(ctx context.Context, key, profile string, input generated.ClientSessionRequest) (generated.ClientSession, error) {
	var zero generated.ClientSession
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	v, ok := ctx.Value(integrationContextKey{}).(currentIntegration)
	b, e := json.Marshal(input)
	if !ok || v.store != s || v.principal.Kind != domain.IntegrationService || s.integrationSessionRoot == nil || s.integrationClosed.Load() || s.Guard == nil || domain.ValidateIdempotencyKey(key) != nil || e != nil || generated.Validate("ClientSessionRequest", b) != nil || input.ProductID != v.principal.Product || (profile != "public_v1" && profile != "integration_r0_v1") {
		return zero, ErrDenied
	}
	input = creationCopy(input)
	p := v.principal
	ctx, id, release, e := s.EnterConfiguration(ctx)
	if e != nil {
		return zero, e
	}
	defer release()
	ctx, done, e := s.EnterOperation(ctx)
	if e != nil {
		return zero, e
	}
	defer done()
	eligibility, e := s.sessionEligibility(ctx, s.DB, v, input)
	if e != nil {
		return zero, e
	}
	if eligibility.identity.Installation != id.Installation || eligibility.identity.Generation != id.Generation {
		return zero, ErrDenied
	}
	hashInput, _ := json.Marshal(struct {
		Profile string
		Request generated.ClientSessionRequest
	}{profile, input})
	payload := sha256.Sum256(hashInput)
	r, e := sessionRead(ctx, s.DB, p, key)
	replay := e == nil
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return zero, e
	}
	if replay && r.Hash != hex.EncodeToString(payload[:]) {
		return zero, ErrConflict
	}
	var referenced bool
	if e = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM integration_principals WHERE kind='CLIENT')").Scan(&referenced); e != nil {
		return zero, e
	}
	material, e := s.integrationSessionRoot.SessionHMACKey(id.Installation, !referenced && !replay)
	if e != nil {
		return zero, ErrDenied
	}
	kh := sha256.Sum256(append([]byte(id.Installation+"/1/"), material[:]...))
	keyref := hex.EncodeToString(kh[:])
	if !replay {
		var now time.Time
		if e = s.DB.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
			return zero, e
		}
		var upper []string
		if json.Unmarshal([]byte(eligibility.entitlement.Actions), &upper) != nil {
			return zero, ErrDenied
		}
		actions := []string{}
		for _, a := range input.RequestedActions {
			for _, u := range upper {
				if a == u {
					actions = append(actions, a)
					break
				}
			}
		}
		sort.Strings(actions)
		if len(actions) == 0 {
			return zero, ErrDenied
		}
		r = sessionRecord{ID: creationID(p.Tenant, p.Product, p.PrincipalID, "session", key), Installation: id.Installation, Tenant: p.Tenant, Product: p.Product, Customer: p.Customer, Service: p.PrincipalID, ServiceSubject: p.StableSubject, Player: eligibility.entitlement.Player, World: input.WorldInstanceID, External: input.ExternalSubjectID, Entitlement: input.EntitlementReference, Profile: profile, Key: key, Hash: hex.EncodeToString(payload[:]), KeyRef: keyref, KeyVersion: 1, Receipt: creationID(p.Tenant, p.Product, p.PrincipalID, "session-receipt", key), Actions: actions, Nonce: make([]byte, 32), Issued: now.UTC(), Expires: now.UTC().Add(15 * time.Minute), EntitlementRevision: eligibility.entitlement.Revision, MappingRevision: eligibility.mapping.MappingRevision, CustomerRevision: eligibility.mapping.CustomerRevision}
		if _, e = rand.Read(r.Nonce); e != nil {
			return zero, e
		}
	}
	if r.KeyRef != keyref {
		return zero, ErrDenied
	}
	token := sessionToken(material, r)
	tokenHash := sha256.Sum256([]byte(token))
	keys := append(sessionKeys(p, r, eligibility.keys), credentialHashKey(tokenHash))
	tx, e := s.integrationMemoryReadTx(ctx, keys)
	if e != nil {
		return zero, e
	}
	defer tx.Rollback()
	check := func() error {
		if s.integrationClosed.Load() {
			return ErrDenied
		}
		if e := checkConfigurationInstallationTx(ctx, tx, id); e != nil {
			return e
		}
		now, e := s.sessionEligibility(ctx, tx, v, input)
		if e != nil {
			return e
		}
		if !reflect.DeepEqual(now, eligibility) || r.EntitlementRevision != now.entitlement.Revision || r.MappingRevision != now.mapping.MappingRevision || r.CustomerRevision != now.mapping.CustomerRevision {
			return ErrDenied
		}
		return nil
	}
	if e = check(); e != nil {
		return zero, e
	}
	old, e := sessionRead(ctx, tx, p, key)
	if e == nil {
		if old.Hash != r.Hash {
			return zero, ErrConflict
		}
		r = old
		if r.KeyRef != keyref {
			return zero, ErrDenied
		}
		token = sessionToken(material, r)
		tokenHash = sha256.Sum256([]byte(token))
		if e = s.currentSessionRow(ctx, tx, r, tokenHash); e != nil {
			return zero, e
		}
	} else {
		if !errors.Is(e, sql.ErrNoRows) || replay {
			return zero, ErrDenied
		}
		if e = reserveCredentialHashTx(ctx, tx, tokenHash, "CLIENT", r.ID, id.Installation, false); e != nil {
			return zero, e
		}
		actions, _ := json.Marshal(r.Actions)
		result, e := tx.ExecContext(ctx, `INSERT INTO integration_principals(principal_id,token_hash,kind,tenant_id,product_id,customer_id,subject_id,world_instance_id,issuer_service_key,profile,client_actions,entitlement_id,issued_at,expires_at,revoked,revision,nonce,key_version,key_ref,issuance_receipt) VALUES(?,?,'CLIENT',?,?,?,?,?,?,?,?,?,?,?,0,1,?,?,?,?)`, r.ID, tokenHash[:], r.Tenant, r.Product, r.Customer, r.Player, r.World, r.Service, r.Profile, actions, r.Entitlement, r.Issued, r.Expires, r.Nonce, r.KeyVersion, r.KeyRef, r.Receipt)
		if e != nil {
			return zero, e
		}
		if e = registryAffected(result); e != nil {
			return zero, e
		}
		metadata, _ := json.Marshal(r)
		result, e = tx.ExecContext(ctx, `INSERT INTO integration_receipts(tenant_id,product_id,owner_id,operation,scope_kind,scope_address,original_key,receipt_ref,payload_hash,result_metadata,state,target_revision,committed_at) VALUES(?,?,?,'EXCHANGE_SESSION','SESSION',?,?,?,?,?,'COMMITTED',1,UTC_TIMESTAMP(6))`, p.Tenant, p.Product, p.PrincipalID, p.Product, key, r.Receipt, payload[:], metadata)
		if e != nil {
			return zero, e
		}
		if e = registryAffected(result); e != nil {
			return zero, e
		}
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(ctx, tx); e != nil {
			return zero, e
		}
	}
	if e = check(); e != nil {
		return zero, e
	}
	if e = s.currentSessionRow(ctx, tx, r, tokenHash); e != nil {
		return zero, e
	}
	if e = s.commitRegistry(ctx, tx); e != nil {
		return zero, e
	}
	return generated.ClientSession{AccessToken: token, AllowedActions: append([]string(nil), r.Actions...), ExpiresAt: r.Expires.Format(time.RFC3339Nano), Scope: generated.WorldScope{TenantID: r.Tenant, ProductID: r.Product, WorldInstanceID: r.World}, SubjectID: r.Player, ViewEpoch: strconv.FormatUint(eligibility.mapping.View, 10)}, nil
}
