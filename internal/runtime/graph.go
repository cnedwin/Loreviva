package runtime

import (
	"encoding/json"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/generated"
)

// Runtime graph references keep their defining release namespace. Loading a
// community prop never grants its owner, generation route, or action authority.
type runtimeProp struct {
	Ref           generated.DefinitionRef
	SourceRelease string
	Definition    generated.PropDefinition
	Rules         []generated.RuleDefinition
}
type runtimeGraph struct {
	root    generated.Release
	catalog map[string]generated.Release
	allowed map[string]bool
}

func localRef(id string) generated.DefinitionRef {
	b, _ := json.Marshal(map[string]string{"kind": "LOCAL_DRAFT", "object_id": id})
	return b
}
func (g runtimeGraph) resolve(ref generated.DefinitionRef, owner string) (json.RawMessage, generated.DefinitionRef, string, error) {
	var r struct {
		Kind        string `json:"kind"`
		ObjectID    string `json:"object_id"`
		ReleaseID   string `json:"release_id"`
		Version     string `json:"version"`
		ContentHash string `json:"content_hash"`
	}
	if e := json.Unmarshal(ref, &r); e != nil {
		return nil, nil, "", e
	}
	if r.Kind == "PINNED_DEFINITION" {
		h, e := content.Hash(ref)
		if e != nil || !g.allowed[h] {
			return nil, nil, "", content.ErrBlocked
		}
		owner = r.ReleaseID
	} else if r.Kind != "LOCAL_DRAFT" {
		return nil, nil, "", content.ErrBlocked
	}
	source, ok := g.catalog[owner]
	if !ok {
		return nil, nil, "", content.ErrBlocked
	}
	for _, raw := range source.Input.Objects {
		var head struct {
			ObjectID string `json:"object_id"`
			Version  string `json:"version"`
		}
		if e := json.Unmarshal(raw, &head); e != nil {
			return nil, nil, "", e
		}
		if head.ObjectID != r.ObjectID {
			continue
		}
		h, e := content.Hash(json.RawMessage(raw))
		if e != nil {
			return nil, nil, "", e
		}
		if r.Kind == "PINNED_DEFINITION" && (r.Version != head.Version || r.ContentHash != h) {
			return nil, nil, "", content.ErrBlocked
		}
		canonical := localRef(r.ObjectID)
		if owner != string(g.root.ReleaseID) {
			canonical, e = json.Marshal(map[string]string{"kind": "PINNED_DEFINITION", "release_id": owner, "object_id": r.ObjectID, "version": head.Version, "content_hash": h})
			if e != nil {
				return nil, nil, "", e
			}
		}
		return raw, canonical, owner, nil
	}
	return nil, nil, "", content.ErrBlocked
}
func collectRuntimeProps(root generated.Release, catalog map[string]generated.Release) ([]runtimeProp, error) {
	g := runtimeGraph{root: root, catalog: catalog, allowed: map[string]bool{}}
	for _, d := range root.DependencyLocks {
		b, _ := json.Marshal(map[string]string{"kind": "PINNED_DEFINITION", "release_id": string(d.ReleaseID), "object_id": string(d.ObjectID), "version": string(d.Version), "content_hash": d.ContentHash})
		h, e := content.Hash(json.RawMessage(b))
		if e != nil {
			return nil, e
		}
		g.allowed[h] = true
	}
	props := []runtimeProp{}
	seen := map[string]bool{}
	active := map[string]bool{}
	nodes := 0
	var visit func(generated.DefinitionRef, string, int) error
	visit = func(ref generated.DefinitionRef, owner string, depth int) error {
		if depth > 32 || nodes >= 1000 {
			return content.ErrBlocked
		}
		raw, canonical, source, e := g.resolve(ref, owner)
		if e != nil {
			return e
		}
		key, e := content.Hash(canonical)
		if e != nil {
			return e
		}
		if active[key] {
			return content.ErrBlocked
		}
		if seen[key] {
			return nil
		}
		nodes++
		active[key] = true
		var head struct {
			Kind string `json:"kind"`
		}
		if e = json.Unmarshal(raw, &head); e != nil {
			return e
		}
		switch head.Kind {
		case "SCENE":
			var scene generated.SceneDefinition
			if e = generated.Decode("SceneDefinition", raw, &scene); e != nil {
				return e
			}
			for _, child := range scene.ChildRefs {
				if e = visit(child, source, depth+1); e != nil {
					return e
				}
			}
			for _, adjacent := range scene.AdjacentSceneRefs {
				b, _, _, x := g.resolve(adjacent, source)
				if x != nil {
					return x
				}
				var h struct {
					Kind string `json:"kind"`
				}
				_ = json.Unmarshal(b, &h)
				if h.Kind != "SCENE" {
					return content.ErrBlocked
				}
				if e = visit(adjacent, source, depth+1); e != nil {
					return e
				}
			}
		case "PROP":
			var p generated.PropDefinition
			if e = generated.Decode("PropDefinition", raw, &p); e != nil {
				return e
			}
			scene, _, _, x := g.resolve(p.SceneRef, source)
			if x != nil {
				return x
			}
			var h struct {
				Kind string `json:"kind"`
			}
			_ = json.Unmarshal(scene, &h)
			if h.Kind != "SCENE" {
				return content.ErrBlocked
			}
			item := runtimeProp{Ref: canonical, SourceRelease: source, Definition: p, Rules: []generated.RuleDefinition{}}
			for _, rr := range p.RuleRefs {
				b, _, ruleSource, x := g.resolve(rr, source)
				if x != nil {
					return x
				}
				var rule generated.RuleDefinition
				if x = generated.Decode("RuleDefinition", b, &rule); x != nil {
					return x
				}
				// The primitive consumes local target IDs. Prove that each target actually
				// denotes this qualified prop before giving it those IDs.
				for _, target := range rule.Targets {
					_, targetRef, _, x := g.resolve(target, ruleSource)
					if x != nil {
						return x
					}
					targetKey, x := content.Hash(targetRef)
					if x != nil || targetKey != key {
						return content.ErrBlocked
					}
				}
				item.Rules = append(item.Rules, rule)
			}
			props = append(props, item)
		default:
			return content.ErrBlocked
		}
		active[key] = false
		seen[key] = true
		return nil
	}
	// Preserve legacy local prop materialization while also following supported
	// scene child/adjacency references into explicitly pinned immutable releases.
	for _, raw := range root.Input.Objects {
		var h struct {
			Kind     string `json:"kind"`
			ObjectID string `json:"object_id"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return nil, e
		}
		if h.Kind == "PROP" || h.Kind == "SCENE" {
			if e := visit(localRef(h.ObjectID), string(root.ReleaseID), 0); e != nil {
				return nil, e
			}
		}
		if h.Kind == "WORLD" && h.ObjectID == string(root.Input.RootObjectID) {
			var w generated.WorldDefinition
			if e := json.Unmarshal(raw, &w); e != nil {
				return nil, e
			}
			if e := visit(w.InitialSceneRef, string(root.ReleaseID), 0); e != nil {
				return nil, e
			}
		}
	}
	return props, nil
}
func runtimeItemID(tenant, product, world string, p runtimeProp) string {
	var ref struct {
		Kind string `json:"kind"`
	}
	_ = json.Unmarshal(p.Ref, &ref)
	if ref.Kind == "LOCAL_DRAFT" {
		return content.Identity(tenant, product, world, "item", string(p.Definition.ObjectID))
	}
	h, _ := content.Hash(p.Ref)
	return content.Identity(tenant, product, world, "item", "pinned", h)
}
