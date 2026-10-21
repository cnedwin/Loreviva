package content

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
)

const integrationInitialPolicy = "INTEGRATION_INITIAL_PUBLISHED_PRIVATE_V1"

type initialSourceInspector struct {
	catalog map[string]generated.Release
	seen    map[string]bool
}

func initialPolicyError(pointer string) error {
	return &FieldErrors{Findings: []generated.FieldError{{Pointer: pointer, Code: "INTEGRATION_INITIAL_SOURCE_UNSUPPORTED", Message: "Initial source lacks the supported current publication provenance."}}}
}
func initialBodyHash(body string) string {
	h := sha256.Sum256([]byte(body))
	return hex.EncodeToString(h[:])
}

// Publication ownership/currentness is proved by the Store before this sealed
// inspection. Authored flags alone never enter here as an access credential.
func (s *Service) InspectIntegrationInitialSourcesTx(ctx context.Context, tx store.Tx, plan *store.IntegrationCreationPlan) (*store.IntegrationInitialLineage, error) {
	if e := s.Store.CheckIntegrationCreationTx(ctx, tx, plan); e != nil {
		return nil, e
	}
	root := plan.Release()
	scope := plan.Scope()
	player := plan.PlayerScope()
	in := initialSourceInspector{catalog: map[string]generated.Release{string(root.ReleaseID): root}, seen: map[string]bool{}}
	for _, r := range plan.Dependencies() {
		in.catalog[string(r.ReleaseID)] = r
	}
	for _, r := range in.catalog {
		if e := tx.RequireLock(Key(scope.ProductScope, "content_release", string(r.ReleaseID))); e != nil {
			return nil, e
		}
		if _, e := ResolvePinned(ctx, tx, scope.ProductScope, r.Input); e != nil {
			return nil, e
		}
	}
	return InspectIntegrationPublishedLineage(root, plan.Dependencies(), scope, player, plan.CustomerID(), plan.WorldID())
}

// This pure projection carries provenance only; the caller must establish
// current publication and owning authority separately, in its own read Tx.
func InspectIntegrationPublishedLineage(root generated.Release, dependencies []generated.Release, scope, player domain.Scope, customer, worldID string) (*store.IntegrationInitialLineage, error) {
	in := initialSourceInspector{catalog: map[string]generated.Release{string(root.ReleaseID): root}, seen: map[string]bool{}}
	for _, r := range dependencies {
		in.catalog[string(r.ReleaseID)] = r
	}
	raw, e := in.object(string(root.ReleaseID), string(root.Input.RootObjectID))
	if e != nil {
		return nil, e
	}
	worldOrigin, e := in.policy(string(root.ReleaseID), raw, "/world", false)
	if e != nil {
		return nil, e
	}
	var world generated.WorldDefinition
	if generated.Decode("WorldDefinition", raw, &world) != nil {
		return nil, initialPolicyError("/world")
	}
	out := &store.IntegrationInitialLineage{Policy: integrationInitialPolicy, Tenant: scope.TenantID, Product: scope.ProductID, World: scope.WorldInstanceID, Customer: customer, Player: player.SubjectID, CreationReceipt: worldID, Sources: []store.IntegrationInitialSource{}}
	for actorIndex, setup := range world.InitialActorSetups {
		ptr := "/initial_actor_setups/" + strconv.Itoa(actorIndex)
		characterRaw, owner, e := in.resolve(string(root.ReleaseID), setup.CharacterRef)
		if e != nil {
			return nil, e
		}
		characterOrigin, e := in.policy(owner, characterRaw, ptr+"/character_ref", false)
		if e != nil {
			return nil, e
		}
		var character generated.CharacterDefinition
		if generated.Decode("CharacterDefinition", characterRaw, &character) != nil {
			return nil, initialPolicyError(ptr + "/character_ref")
		}
		actor := Identity(scope.TenantID, scope.ProductID, scope.WorldInstanceID, "actor", string(setup.SlotID))
		origins := []store.IntegrationSourceOrigin{worldOrigin, characterOrigin}
		coreHash, e := Hash(json.RawMessage(characterRaw))
		if e != nil {
			return nil, e
		}
		out.Sources = append(out.Sources, store.IntegrationInitialSource{Actor: actor, SourceID: Identity(worldID, actor, "core"), Acquisition: worldID, BodyHash: coreHash, Kind: "CORE", Origins: origins})
		goals := append(append([]string{}, setup.InitialGoals...), character.LongTermGoals...)
		for index, goal := range goals {
			sourceOrigins := append([]store.IntegrationSourceOrigin{}, origins...)
			pointer := ptr + "/initial_goals/" + strconv.Itoa(index)
			if index >= len(setup.InitialGoals) {
				pointer = "/long_term_goals/" + strconv.Itoa(index-len(setup.InitialGoals))
			}
			if index < len(setup.InitialGoals) {
				sourceOrigins[0].Pointer = pointer
			} else {
				sourceOrigins[1].Pointer = pointer
			}
			out.Sources = append(out.Sources, store.IntegrationInitialSource{Actor: actor, SourceID: Identity(worldID, actor, "goal", strconv.Itoa(index)), Acquisition: worldID, BodyHash: initialBodyHash(goal), Kind: "GOAL", Origins: sourceOrigins})
		}
		refs := append([]generated.DefinitionRef{}, setup.KnowledgeRefs...)
		for n, ref := range append(refs, character.InitialKnowledgeRefs...) {
			local := string(root.ReleaseID)
			if n >= len(setup.KnowledgeRefs) {
				local = owner
			}
			note, release, e := in.resolve(local, ref)
			if e != nil {
				return nil, e
			}
			chain, e := in.noteOrigins(release, note, ptr+"/knowledge/"+strconv.Itoa(n), map[string]bool{}, 0)
			if e != nil {
				return nil, e
			}
			var knowledge generated.KnowledgeDefinition
			if generated.Decode("KnowledgeDefinition", note, &knowledge) != nil {
				return nil, initialPolicyError(ptr)
			}
			out.Sources = append(out.Sources, store.IntegrationInitialSource{Actor: actor, SourceID: Identity("initial-knowledge", release, string(knowledge.ObjectID), string(knowledge.Version)), Acquisition: worldID, BodyHash: initialBodyHash(knowledge.Text), Kind: "BELIEF", Origins: append(append([]store.IntegrationSourceOrigin{}, origins...), chain...)})
		}
	}
	return out, nil
}
func (in *initialSourceInspector) object(release, id string) (json.RawMessage, error) {
	r, ok := in.catalog[release]
	if !ok {
		return nil, store.ErrReplan
	}
	for _, raw := range r.Input.Objects {
		var h struct {
			ID string `json:"object_id"`
		}
		if json.Unmarshal(raw, &h) != nil {
			return nil, initialPolicyError("/objects")
		}
		if h.ID == id {
			return raw, nil
		}
	}
	return nil, initialPolicyError("/objects")
}
func (in *initialSourceInspector) resolve(local string, ref generated.DefinitionRef) (json.RawMessage, string, error) {
	var v struct {
		Kind, Version string
		ReleaseID     string `json:"release_id"`
		ObjectID      string `json:"object_id"`
		Hash          string `json:"content_hash"`
	}
	if json.Unmarshal(ref, &v) != nil {
		return nil, "", initialPolicyError("/reference")
	}
	release := local
	if v.Kind == "PINNED_DEFINITION" {
		allowed := false
		for _, d := range in.catalog[local].DependencyLocks {
			if string(d.ReleaseID) == v.ReleaseID && string(d.ObjectID) == v.ObjectID && string(d.Version) == v.Version && d.ContentHash == v.Hash {
				allowed = true
			}
		}
		if !allowed {
			return nil, "", initialPolicyError("/reference")
		}
		release = v.ReleaseID
	} else if v.Kind != "LOCAL_DRAFT" {
		return nil, "", initialPolicyError("/reference")
	}
	raw, e := in.object(release, v.ObjectID)
	if e != nil {
		return nil, "", e
	}
	if v.Kind == "PINNED_DEFINITION" {
		var h struct{ Version string }
		_ = json.Unmarshal(raw, &h)
		hash, e := Hash(json.RawMessage(raw))
		if e != nil || hash != v.Hash || h.Version != v.Version {
			return nil, "", initialPolicyError("/reference")
		}
	}
	return raw, release, nil
}
func (in *initialSourceInspector) policy(release string, raw json.RawMessage, pointer string, note bool) (store.IntegrationSourceOrigin, error) {
	var zero store.IntegrationSourceOrigin
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return zero, initialPolicyError(pointer)
	}
	var h struct {
		ID         string `json:"object_id"`
		Version    string
		Kind       string
		Audience   string `json:"audience_policy"`
		Rights     generated.SourceRights
		SourceRefs []string `json:"source_refs"`
	}
	if json.Unmarshal(raw, &h) != nil || h.Audience != "PRODUCT_READERS" || h.Rights.OriginKind != "SYNTHETIC" && h.Rights.OriginKind != "OWNED" || h.Rights.License == "" || h.Rights.SourceLabel == "" || h.SourceRefs == nil || len(h.SourceRefs) != 0 {
		return zero, initialPolicyError(pointer)
	}
	var rights map[string]json.RawMessage
	if json.Unmarshal(fields["rights"], &rights) != nil {
		return zero, initialPolicyError(pointer + "/rights")
	}
	if _, present := rights["expires_at"]; present {
		return zero, initialPolicyError(pointer + "/rights/expires_at")
	}
	if _, present := rights["source_location"]; present {
		return zero, initialPolicyError(pointer + "/rights/source_location")
	}
	for _, key := range []string{"source_asset_id", "source_asset_hash", "extraction_report_id", "source_location"} {
		if _, present := fields[key]; present {
			return zero, initialPolicyError(pointer + "/" + key)
		}
	}
	for _, want := range []string{"COPY", "PUBLISH", "REDISTRIBUTE"} {
		found := false
		for _, use := range h.Rights.AllowedUses {
			found = found || use == want
		}
		if !found {
			return zero, initialPolicyError(pointer + "/rights/allowed_uses")
		}
	}
	if !note {
		if _, present := fields["origin_ref"]; present {
			return zero, initialPolicyError(pointer + "/origin_ref")
		}
	}
	hash, e := Hash(json.RawMessage(raw))
	if e != nil {
		return zero, e
	}
	key := release + "/" + h.ID + "/" + h.Version
	in.seen[key] = true
	if len(in.seen) > 200 {
		return zero, initialPolicyError(pointer)
	}
	return store.IntegrationSourceOrigin{Release: release, Object: h.ID, Version: h.Version, Hash: hash, Pointer: pointer, Rights: h.Rights, Audience: h.Audience, SourceRefs: []string{}, AssetAbsent: true}, nil
}
func (in *initialSourceInspector) noteOrigins(release string, raw json.RawMessage, pointer string, active map[string]bool, depth int) ([]store.IntegrationSourceOrigin, error) {
	if depth >= 8 {
		return nil, initialPolicyError(pointer + "/origin_ref")
	}
	var note generated.KnowledgeDefinition
	if generated.Decode("KnowledgeDefinition", raw, &note) != nil || note.KnowledgeType != "NOTE" {
		return nil, initialPolicyError(pointer + "/knowledge_type")
	}
	id := fmt.Sprintf("%s/%s/%s", release, note.ObjectID, note.Version)
	if active[id] {
		return nil, initialPolicyError(pointer + "/origin_ref")
	}
	active[id] = true
	defer delete(active, id)
	origin, e := in.policy(release, raw, pointer, true)
	if e != nil {
		return nil, e
	}
	out := []store.IntegrationSourceOrigin{origin}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if ref, ok := fields["origin_ref"]; ok {
		parent, parentRelease, e := in.resolve(release, generated.DefinitionRef(ref))
		if e != nil {
			return nil, e
		}
		var other generated.KnowledgeDefinition
		if generated.Decode("KnowledgeDefinition", parent, &other) != nil || note.Text != other.Text {
			return nil, initialPolicyError(pointer + "/origin_ref")
		}
		for _, use := range note.Rights.AllowedUses {
			found := false
			for _, allowed := range other.Rights.AllowedUses {
				found = found || allowed == use
			}
			if !found {
				return nil, initialPolicyError(pointer + "/rights")
			}
		}
		chain, e := in.noteOrigins(parentRelease, parent, pointer+"/origin_ref", active, depth+1)
		if e != nil {
			return nil, e
		}
		out = append(out, chain...)
	}
	return out, nil
}
