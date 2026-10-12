package content

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"strconv"
)

// Fixed internal adapter signatures; these are not public wire schemas.
func ValidateOnlineSkillAdapter(skill generated.SkillDefinition, b generated.SkillBinding) error {
	if b.BudgetOwner != "PARENT" || skill.FailurePolicy != "BLOCK" || skill.MaxDepth < 1 || skill.MaxDepth > 4 || skill.MaxCostUnits != "0" || len(skill.ToolIDS) != 0 || len(skill.ConflictingRefs) != 0 || len(skill.RegisteredCapabilityIDS) != 1 || skill.Generation.Mode != "NONE" {
		return ErrBlocked
	}
	n, e := strconv.ParseUint(string(skill.MaxDurationMs), 10, 64)
	if e != nil || n == 0 || n > 120000 {
		return ErrBlocked
	}
	switch skill.RegisteredCapabilityIDS[0] {
	case "TEXT":
		if b.Trigger != "TURN" || skill.ImplementationKind != "GENERATIVE" || skill.InputSchemaID != "GenerationMaterial" || skill.OutputSchemaID != "DispatchAuthorization" {
			return ErrBlocked
		}
	case "SIGN_CUSTODY":
		if b.Trigger != "PRECHECK" || skill.ImplementationKind != "PROGRAM" || skill.InputSchemaID != "CandidateActions" || skill.OutputSchemaID != "ActionDecision" {
			return ErrBlocked
		}
	default:
		return ErrBlocked
	}
	return nil
}

// Creation preflight checks the actual selected module binding before receipt
// or reservation. Runtime repeats current qualified source proof at execution.
func validateRehearsalSkills(ctx context.Context, tx store.Tx, scope domain.ProductScope, in generated.DraftInput, resolved ResolvedDefinitions) error {
	for _, module := range in.ModuleAgentBindings {
		if !module.Enabled || module.ModuleID != "REHEARSAL" || module.OwnerObjectID != in.RootObjectID {
			continue
		}
		raw, e := ResolveDefinition(module.RoleRef, in, resolved)
		if e != nil {
			return e
		}
		var role generated.RoleDefinition
		if generated.Decode("RoleDefinition", raw, &role) != nil {
			return ErrBlocked
		}
		defining, e := skillDefiningInputTx(ctx, tx, scope, in, module.RoleRef)
		if e != nil {
			return e
		}
		count := 0
		seen := map[generated.Identity]bool{}
		for _, b := range defining.SkillBindings {
			if !b.Enabled || b.RoleObjectID != role.ObjectID {
				continue
			}
			count++
			if count > 8 || seen[b.BindingID] {
				return ErrBlocked
			}
			seen[b.BindingID] = true
			raw, e := ResolveDefinition(b.SkillRef, defining, resolved)
			if e != nil {
				return e
			}
			var skill generated.SkillDefinition
			if generated.Decode("SkillDefinition", raw, &skill) != nil {
				return ErrBlocked
			}
			if e = ValidateOnlineSkillAdapter(skill, b); e != nil {
				return e
			}
			found := false
			want, _ := Hash(json.RawMessage(raw))
			for _, ref := range role.SkillRefs {
				rr, e := ResolveDefinition(ref, defining, resolved)
				if e != nil {
					return e
				}
				h, _ := Hash(json.RawMessage(rr))
				if h == want {
					found = true
				}
			}
			if !found {
				return ErrBlocked
			}
		}
	}
	return nil
}

// The caller already resolved the exact role and all declared dependencies.
// Only its actual defining release owns SkillBindings and local Skill refs.
func skillDefiningInputTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, in generated.DraftInput, ref generated.DefinitionRef) (generated.DraftInput, error) {
	var identity struct {
		Kind    string `json:"kind"`
		Release string `json:"release_id"`
	}
	if json.Unmarshal(ref, &identity) != nil {
		return generated.DraftInput{}, ErrBlocked
	}
	if identity.Kind == "LOCAL_DRAFT" {
		return in, nil
	}
	if identity.Kind != "PINNED_DEFINITION" || identity.Release == "" {
		return generated.DraftInput{}, ErrBlocked
	}
	if e := tx.RequireLock(Key(scope, "content_release", identity.Release)); e != nil {
		return generated.DraftInput{}, e
	}
	var raw, storedHash []byte
	if e := tx.QueryRowContext(ctx, "SELECT c.document,c.content_hash FROM content_releases c JOIN releases r USING(tenant_id,product_id,release_id) WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=? AND r.available=1", scope.TenantID, scope.ProductID, identity.Release).Scan(&raw, &storedHash); e != nil {
		return generated.DraftInput{}, e
	}
	var release generated.Release
	if generated.Decode("Release", raw, &release) != nil {
		return generated.DraftInput{}, ErrBlocked
	}
	hash, e := Hash(struct {
		Input        generated.DraftInput
		Dependencies []generated.DependencyLock
		Mappings     any
	}{release.Input, release.DependencyLocks, release.RouteMappings})
	if e != nil {
		return generated.DraftInput{}, e
	}
	if release.Scope != Scope(scope) || string(release.ReleaseID) != identity.Release || hash != release.ContentHash || hex.EncodeToString(storedHash) != hash {
		return generated.DraftInput{}, ErrBlocked
	}
	return release.Input, nil
}
