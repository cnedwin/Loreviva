package runtime

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
)

type GenerationRoute struct {
	ReleaseID, ActorID, Module, Connection, Model, DescriptorHash, SettingsHash string
	OwnerReference                                                              generated.DefinitionRef
	Settings                                                                    generated.GenerationSettings
}

// GenerationRouteTx resolves the actual immutable current release mapping for
// this materialized actor, using the qualified owner and module together.
func GenerationRouteTx(ctx context.Context, tx store.Tx, s domain.Scope, actorID string) (GenerationRoute, error) {
	if route, e := rehearsalGenerationRouteTx(ctx, tx, s, actorID); e != nil {
		return GenerationRoute{}, e
	} else if route != nil {
		return *route, nil
	}
	return generationRouteTx(ctx, tx, s, actorID, false)
}
func CommittedGenerationRouteTx(ctx context.Context, tx store.Tx, s domain.Scope, actorID, task string) (GenerationRoute, error) {
	retired, e := tx.AssertCommittedHistory(ctx, s, task)
	if e != nil {
		return GenerationRoute{}, e
	}
	return generationRouteTx(ctx, tx, s, actorID, retired)
}
func generationRouteTx(ctx context.Context, tx store.Tx, s domain.Scope, actorID string, history bool) (GenerationRoute, error) {
	var out GenerationRoute
	if e := tx.RequireLock(worldKey(s)); e != nil {
		return out, e
	}
	if e := tx.RequireLock(resourceKey(s, "actor", actorID)); e != nil {
		return out, e
	}
	var raw []byte
	if e := tx.QueryRowContext(ctx, "SELECT document FROM actors WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND actor_id=?", s.TenantID, s.ProductID, s.WorldInstanceID, actorID).Scan(&raw); e != nil {
		return out, e
	}
	var actor ActorState
	if e := json.Unmarshal(raw, &actor); e != nil {
		return out, e
	}
	var release string
	if e := tx.QueryRowContext(ctx, "SELECT release_id FROM world_controls WHERE tenant_id=? AND product_id=? AND world_instance_id=? AND (status='RUNNING' OR (? AND status='PAUSED'))", s.TenantID, s.ProductID, s.WorldInstanceID, history).Scan(&release); e != nil {
		return out, e
	}
	if history {
		if e := tx.RequireLock(content.Key(s.ProductScope, "content_release", release)); e != nil {
			return out, e
		}
	}
	if e := tx.QueryRowContext(ctx, "SELECT c.document FROM content_releases c JOIN releases r ON r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.release_id=c.release_id WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=? AND r.available=1", s.TenantID, s.ProductID, release).Scan(&raw); e != nil {
		return out, e
	}
	var rel generated.Release
	if e := generated.Decode("Release", raw, &rel); e != nil {
		return out, e
	}
	ownerHash, e := content.Hash(json.RawMessage(actor.OwnerReference))
	if e != nil {
		return out, e
	}
	found := 0
	for _, m := range rel.RouteMappings {
		hash, e := content.Hash(json.RawMessage(m.OwnerReference))
		if e != nil {
			return out, e
		}
		if m.ModuleID != "INTERACTION" || hash != ownerHash {
			continue
		}
		found++
		settings, e := content.Hash(m.EffectiveSettings)
		if e != nil {
			return out, e
		}
		descriptor, _ := content.Hash(struct{ Connection, Model, Price string }{content.MockConnection, content.MockModel, "0"})
		if m.ConnectionRef != content.MockConnection || m.ModelID != content.MockModel || settings != m.SettingsHash || m.DescriptorHash != descriptor || m.EffectiveSettings.Mode != "OVERRIDE" {
			return out, store.ErrDenied
		}
		out = GenerationRoute{ReleaseID: release, ActorID: actorID, Module: "INTERACTION", Connection: string(m.ConnectionRef), Model: string(m.ModelID), DescriptorHash: m.DescriptorHash, SettingsHash: m.SettingsHash, OwnerReference: append([]byte{}, m.OwnerReference...), Settings: m.EffectiveSettings}
	}
	if found != 1 {
		return GenerationRoute{}, store.ErrDenied
	}
	return out, nil
}
