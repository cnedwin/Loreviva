package runtime

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"sort"
	"strconv"
	"time"
)

// InspectTurnSkillsTx reuses the original actor/release and fixed rehearsal
// authority. Every release lock was precollected by the generation owner.
func (r *Runtime) InspectTurnSkillsTx(ctx context.Context, tx store.Tx, s domain.Scope, actor string) ([]domain.TurnSkill, error) {
	return TurnSkillsTx(ctx, tx, s, actor)
}
func TurnSkillsTx(ctx context.Context, tx store.Tx, s domain.Scope, actor string) ([]domain.TurnSkill, error) {
	inputs, e := TurnSkillInputsTx(ctx, tx, s, actor)
	if e != nil {
		return nil, e
	}
	out := []domain.TurnSkill{}
	for _, in := range inputs {
		out = append(out, in.TurnSkill)
	}
	return out, nil
}
func TurnSkillInputsTx(ctx context.Context, tx store.Tx, s domain.Scope, actor string) ([]domain.TurnSkillInput, error) {
	init, e := InitializationEvidence(ctx, tx, s)
	if e != nil {
		return nil, e
	}
	var selected *ActorState
	for _, a := range init.Actors {
		if a.ID == actor {
			copy := a
			selected = &copy
		}
	}
	if selected == nil {
		return nil, store.ErrDenied
	}
	basis, synthetic, e := qualifiedRehearsalTx(ctx, tx, s)
	if e != nil {
		return nil, e
	}
	load := func(id string) (generated.Release, error) {
		if e := tx.RequireLock(content.Key(s.ProductScope, "content_release", id)); e != nil {
			return generated.Release{}, e
		}
		if basis != nil && id == string(synthetic.ReleaseID) {
			return synthetic, nil
		}
		var raw []byte
		var rel generated.Release
		if e := tx.QueryRowContext(ctx, "SELECT document FROM content_releases WHERE tenant_id=? AND product_id=? AND release_id=?", s.TenantID, s.ProductID, id).Scan(&raw); e != nil {
			return rel, e
		}
		if e := generated.Decode("Release", raw, &rel); e != nil {
			return rel, e
		}
		if _, e := verifyRelease(ctx, tx, s.ProductScope, rel); e != nil {
			return rel, e
		}
		return rel, nil
	}
	root, e := load(init.ReleaseID)
	if e != nil {
		return nil, e
	}
	g := runtimeGraph{root: root, catalog: map[string]generated.Release{init.ReleaseID: root}, allowed: map[string]bool{}}
	for _, d := range root.DependencyLocks {
		rel, e := load(string(d.ReleaseID))
		if e != nil {
			return nil, e
		}
		g.catalog[string(d.ReleaseID)] = rel
		ref, _ := json.Marshal(map[string]string{"kind": "PINNED_DEFINITION", "release_id": string(d.ReleaseID), "object_id": string(d.ObjectID), "version": string(d.Version), "content_hash": d.ContentHash})
		h, _ := content.Hash(json.RawMessage(ref))
		g.allowed[h] = true
	}
	_, ref, owner, e := g.resolve(selected.OwnerReference, init.ReleaseID)
	if e != nil {
		return nil, e
	}
	var ownerRef struct {
		ObjectID string `json:"object_id"`
	}
	if json.Unmarshal(ref, &ownerRef) != nil {
		return nil, store.ErrDenied
	}
	module := "INTERACTION"
	if basis != nil {
		module = "REHEARSAL"
		owner = init.ReleaseID
		ownerRef.ObjectID = string(root.Input.RootObjectID)
	}
	source := g.catalog[owner]
	var binding *generated.ModuleAgentBinding
	for _, b := range source.Input.ModuleAgentBindings {
		if b.Enabled && b.ModuleID == module && string(b.OwnerObjectID) == ownerRef.ObjectID {
			if binding != nil {
				return nil, store.ErrDenied
			}
			copy := b
			binding = &copy
		}
	}
	if binding == nil {
		return nil, nil
	}
	roleRaw, _, roleSource, e := g.resolve(binding.RoleRef, owner)
	if e != nil {
		return nil, e
	}
	var role generated.RoleDefinition
	if generated.Decode("RoleDefinition", roleRaw, &role) != nil {
		return nil, store.ErrDenied
	}
	roleHash, e := content.Hash(json.RawMessage(roleRaw))
	if e != nil {
		return nil, e
	}
	var now time.Time
	if e = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		return nil, e
	}
	allowed := func(rights generated.SourceRights, audience string) bool {
		if rights.License == "" || (audience != "PRODUCT_EDITORS" && audience != "PRODUCT_READERS") {
			return false
		}
		edit := false
		for _, u := range rights.AllowedUses {
			edit = edit || u == "EDIT"
		}
		if !edit {
			return false
		}
		if rights.ExpiresAt != nil {
			end, e := time.Parse(time.RFC3339, *rights.ExpiresAt)
			if e != nil || !now.Before(end) {
				return false
			}
		}
		return true
	}
	if !allowed(role.Rights, role.AudiencePolicy) {
		return nil, store.ErrDenied
	}
	out := []domain.TurnSkillInput{}
	seen := map[string]bool{}
	// Bindings belong to the exact defining ROLE namespace, not a same-named
	// object in the consumer release.
	for _, b := range g.catalog[roleSource].Input.SkillBindings {
		if !b.Enabled || b.RoleObjectID != role.ObjectID {
			continue
		}
		if len(out) >= 8 || b.BudgetOwner != "PARENT" || seen[string(b.BindingID)] {
			return nil, store.ErrDenied
		}
		seen[string(b.BindingID)] = true
		raw, canonical, origin, e := g.resolve(b.SkillRef, roleSource)
		if e != nil {
			return nil, e
		}
		declared := false
		for _, rr := range role.SkillRefs {
			_, rc, rs, e := g.resolve(rr, roleSource)
			if e != nil {
				return nil, e
			}
			h, _ := content.Hash(rc)
			want, _ := content.Hash(canonical)
			if h == want && rs == origin {
				declared = true
			}
		}
		var skill generated.SkillDefinition
		if !declared || generated.Decode("SkillDefinition", raw, &skill) != nil || !allowed(skill.Rights, skill.AudiencePolicy) || len(skill.ToolIDS) != 0 || len(skill.RegisteredCapabilityIDS) != 1 || skill.MaxCostUnits != "0" || skill.MaxDepth < 1 || skill.MaxDepth > 4 || skill.Generation.Mode != "NONE" || len(skill.ConflictingRefs) != 0 {
			return nil, store.ErrDenied
		}
		if e = content.ValidateOnlineSkillAdapter(skill, b); e != nil {
			return nil, e
		}
		capability := string(skill.RegisteredCapabilityIDS[0])
		if capability == "TEXT" {
			if b.Trigger != "TURN" || skill.ImplementationKind != "GENERATIVE" || skill.InputSchemaID != "GenerationMaterial" || skill.OutputSchemaID != "DispatchAuthorization" {
				return nil, store.ErrDenied
			}
		} else if capability == "SIGN_CUSTODY" {
			if b.Trigger != "PRECHECK" || skill.ImplementationKind != "PROGRAM" || skill.InputSchemaID != "CandidateActions" || skill.OutputSchemaID != "ActionDecision" {
				return nil, store.ErrDenied
			}
		} else {
			return nil, store.ErrDenied
		}
		duration, e := strconv.ParseUint(string(skill.MaxDurationMs), 10, 64)
		if e != nil || duration == 0 || duration > 120000 {
			return nil, store.ErrDenied
		}
		hash, e := content.Hash(json.RawMessage(raw))
		if e != nil {
			return nil, e
		}
		instruction := domain.TurnSkillInput{TurnSkill: domain.TurnSkill{BindingID: string(b.BindingID), DefinitionHash: hash, RoleHash: roleHash, SourceRelease: origin, Capability: capability, InputSchema: string(skill.InputSchemaID), OutputSchema: string(skill.OutputSchemaID), MaxDepth: uint64(skill.MaxDepth), DurationMS: duration, Order: b.Order}, Description: skill.Description, Examples: []domain.SkillExample{}}
		for _, example := range skill.SyntheticExamples {
			instruction.Examples = append(instruction.Examples, domain.SkillExample{Input: example.Input, Expected: example.Expected})
		}
		out = append(out, instruction)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		return out[i].BindingID < out[j].BindingID
	})
	return out, nil
}
