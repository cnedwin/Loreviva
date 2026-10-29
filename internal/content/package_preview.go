package content

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/internal/wepkg"
	"loreviva.local/loreviva/internal/wire"
	"sort"
	"strconv"
	"time"
)

func previewFinding(code string) []generated.FieldError {
	return []generated.FieldError{{Pointer: "/options", Code: generated.Identity(code), Message: "A local dependency, source use or route is missing or incompatible; review the selected mappings."}}
}
func encodePackagePreview(value map[string]any) (generated.PackageImportPreview, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if generated.Validate("PackageImportPreview", raw) != nil {
		return nil, ErrBlocked
	}
	return raw, nil
}
func previewBase() map[string]any {
	return map[string]any{"status": "BLOCKED", "execution_ready": false, "persisted": false, "findings": []generated.FieldError{}, "id_mappings": []generated.PackagePreviewID{}}
}

// PreviewPackage reopens the original archive on every request. Neither a prior
// report nor an idempotency key confers installation or source authority.
func (s *Service) PreviewPackage(ctx context.Context, scope domain.ProductScope, archive, optionsRaw []byte) (generated.PackageImportPreview, error) {
	if len(optionsRaw) > 65536 || generated.Validate("PackagePreviewOptions", optionsRaw) != nil {
		return nil, wire.ErrJSON
	}
	var options packagePreviewOptions
	if json.Unmarshal(optionsRaw, &options) != nil {
		return nil, wire.ErrJSON
	}
	out := previewBase()
	entries, findings := wepkg.PortableEntries(archive, wepkg.DefaultLimits)
	if len(findings) > 0 || len(entries) == 0 {
		out["findings"] = previewFinding("PORTABLE_PACKAGE_INVALID")
		return encodePackagePreview(out)
	}
	entry := options.Entrypoint
	if entry == "" {
		entry = entries[0].LogicalID
	}
	report := wepkg.Check(archive, wepkg.DefaultLimits)
	namespace := options.Namespace
	if namespace == "" {
		namespace = Identity(scope.TenantID, scope.ProductID, scope.SubjectID, report.ArchiveSHA256, "package-preview")
	}
	plan, findings := wepkg.BuildPortablePlan(archive, wepkg.DefaultLimits, entry, namespace)
	if len(findings) > 0 {
		out["findings"] = previewFinding("PORTABLE_ENTRY_INVALID")
		return encodePackagePreview(out)
	}
	out["archive_sha256"] = plan.ArchiveSHA256
	out["plan_sha256"] = plan.PlanSHA256
	out["namespace"] = namespace
	out["entrypoint"] = entry
	if options.Stage == "DISCOVER" {
		requirements, err := packageRequirements(plan, entries)
		if err != nil {
			code := "MAPPING_LIMIT"
			if errors.Is(err, ErrBlocked) {
				code = "MODEL_MAPPING_REQUIRED"
			}
			out["findings"] = previewFinding(code)
			return encodePackagePreview(out)
		}
		read, err := s.Store.BeginOperatorRead(ctx, scope, "content.read", "content.check")
		if err != nil {
			return nil, err
		}
		defer read.Close()
		if err = read.Finish(ctx); err != nil {
			return nil, err
		}
		out["status"] = "NEEDS_MAPPING"
		out["requirements"] = requirements
		return encodePackagePreview(out)
	}
	input, issues := projectPortable(plan, options)
	if len(issues) > 0 {
		out["findings"] = issues
		return encodePackagePreview(out)
	}
	selected := map[string]generated.DependencyLock{}
	for _, m := range options.DependencyMappings {
		selected[m.Pointer] = m.Target
	}
	expectations := map[string][][]string{}
	for _, r := range plan.References {
		if r.Kind != "BUNDLE_OBJECT" {
			d := selected[r.Pointer]
			key := referenceKey(pinReference(d))
			expectations[key] = append(expectations[key], r.ExpectedKinds)
		}
	}
	requiresModel := len(options.ModelMappings) > 0
	for _, cap := range plan.Requirements.Model {
		requiresModel = requiresModel || (cap.Required && cap.CapabilityID == "text")
	}
	inspection, err := s.Store.InspectPackagePreview(ctx, scope, input, plan.Kind, expectations, requiresModel)
	if err != nil {
		if errors.Is(err, ErrBlocked) || errors.Is(err, ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			out["findings"] = previewFinding("LOCAL_DEPENDENCY_UNAVAILABLE")
			return encodePackagePreview(out)
		}
		return nil, err
	}
	if len(inspection.Findings) > 0 {
		out["findings"] = previewFinding("LOCAL_DEFINITION_BLOCKED")
		return encodePackagePreview(out)
	}
	// This digest is an informational snapshot, never a permission or import token.
	hash, err := Hash(struct {
		Scope        domain.ProductScope
		Plan         string
		Options      packagePreviewOptions
		Input        generated.DraftInput
		Route, Basis string
	}{scope, plan.PlanSHA256, options, inspection.Input, inspection.RouteHash, inspection.BasisHash})
	if err != nil {
		return nil, err
	}
	mappings := []generated.PackagePreviewID{}
	for _, id := range plan.IDs {
		mappings = append(mappings, generated.PackagePreviewID{Domain: id.Domain, Owner: id.Owner, LogicalID: id.LogicalID, LocalID: id.LocalID})
	}
	out["status"] = "RESOLVED"
	out["input"] = inspection.Input
	out["id_mappings"] = mappings
	out["preview_sha256"] = hash
	out["route_snapshot_hash"] = inspection.RouteHash
	return encodePackagePreview(out)
}

func packageRequirements(plan wepkg.PortablePlan, entries []wepkg.Entry) (map[string]any, error) {
	root, err := wire.ParseBounded(plan.Body, 1<<20)
	if err != nil {
		return nil, err
	}
	body := root.(map[string]any)
	list := []any{}
	for _, entry := range entries {
		list = append(list, map[string]any{"logical_id": entry.LogicalID, "kind": entry.Kind})
	}
	objects := []any{}
	for _, v := range body["objects"].([]any) {
		o := v.(map[string]any)
		rights := o["rights"].(map[string]any)
		objects = append(objects, map[string]any{"object_id": o["object_id"], "kind": o["kind"], "license": rights["license"], "source_reference": rights["source_reference"], "source_version": o["version"]})
	}
	refs := []any{}
	for _, ref := range plan.References {
		if ref.Kind != "BUNDLE_OBJECT" {
			refs = append(refs, map[string]any{"pointer": ref.Pointer, "kind": ref.Kind, "logical_id": ref.LogicalID, "package_id": ref.PackageID, "package_version": ref.PackageVersion, "content_sha256": ref.ContentSHA256, "expected_kinds": ref.ExpectedKinds})
		}
	}
	files := []any{}
	seen := map[string]bool{}
	for _, file := range plan.Files {
		if seen[file.Path] {
			continue
		}
		seen[file.Path] = true
		files = append(files, map[string]any{"file_path": file.Path, "sha256": file.SHA256, "size_bytes": strconv.FormatInt(file.SizeBytes, 10), "media_type": file.MediaType})
	}
	// Discovery carries the original generation parameters and usage category so
	// ordinary mapping forms never have to unpack JSON or invent missing settings.
	parameters := map[string]any{}
	seenParameters := map[string]bool{}
	generationUses, skillUses := map[string]bool{}, map[string]bool{}
	inspectGeneration := func(value any) error {
		g, ok := value.(map[string]any)
		if !ok || g["mode"] != "REQUIREMENT" {
			return nil
		}
		id := fmt.Sprint(g["capability_id"])
		generationUses[id] = true
		value, has := g["parameters"]
		if prior, seen := seenParameters[id]; seen {
			if prior != has {
				return ErrBlocked
			}
			if has {
				a, _ := Hash(parameters[id])
				b, _ := Hash(value)
				if a != b {
					return ErrBlocked
				}
			}
		}
		seenParameters[id] = has
		if has {
			parameters[id] = value
		}
		return nil
	}
	for _, v := range body["objects"].([]any) {
		o := v.(map[string]any)
		if err := inspectGeneration(o["generation"]); err != nil {
			return nil, err
		}
		if wanted, ok := o["required_capability_ids"].([]any); ok {
			for _, id := range wanted {
				skillUses[fmt.Sprint(id)] = true
			}
		}
	}
	for _, group := range []string{"module_agent_bindings", "skill_bindings"} {
		for _, v := range body[group].([]any) {
			if err := inspectGeneration(v.(map[string]any)["generation"]); err != nil {
				return nil, err
			}
		}
	}
	caps := []any{}
	addCapability := func(id, family string, required bool) {
		usage := []string{}
		if generationUses[id] {
			usage = append(usage, "GENERATION")
		}
		if skillUses[id] {
			usage = append(usage, "SKILL")
		}
		if required {
			usage = append(usage, "REQUIRED_DECLARATION")
		}
		row := map[string]any{"capability_id": id, "family": family, "required": required, "usages": usage}
		if value, has := parameters[id]; has {
			row["parameters"] = value
		}
		caps = append(caps, row)
	}
	for _, v := range plan.Requirements.Model {
		addCapability(v.CapabilityID, "MODEL", v.Required)
	}
	for _, v := range plan.Requirements.Tool {
		addCapability(v.ToolCapabilityID, "TOOL", v.Required)
	}
	for _, v := range plan.Requirements.Presentation {
		addCapability(v.CapabilityID, "PRESENTATION", v.Required)
	}

	if len(refs) > 100 || len(files) > 100 || len(caps) > 1000 {
		return nil, errors.New("package preview mapping capacity exceeded")
	}
	return map[string]any{"entries": list, "objects": objects, "references": refs, "files": files, "capabilities": caps}, nil
}

// Only immutable lock addresses are precollected; current source and audience
// checks are performed again under those same registered locks.
func (s *Service) PlanPackagePreviewLocks(ctx context.Context, scope domain.ProductScope, input generated.DraftInput, kind string) ([]store.LockKey, error) {
	keys, err := s.candidateSourceLocks(ctx, scope, input)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBlocked
	}
	if err != nil {
		return nil, err
	}
	return append(keys, store.RoutePolicyLocks(scope)...), nil
}
func (s *Service) InspectPackagePreviewTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, input generated.DraftInput, kind string, expectations map[string][][]string, requiresModel bool) (store.PackagePreviewInspection, error) {
	empty := store.PackagePreviewInspection{}
	if err := store.CheckOperatorTx(ctx, tx, scope, "content.read", "content.check", "content.edit", "content.copy"); err != nil {
		return empty, err
	}
	prepared, err := Prepare(input)
	if err != nil {
		return empty, ErrBlocked
	}
	resolved, err := ResolvePinned(ctx, tx, scope, prepared)
	if err != nil {
		return empty, err
	}
	for key, sets := range expectations {
		target := resolved[key]
		if target == nil {
			return empty, ErrBlocked
		}
		for _, allowed := range sets {
			if len(allowed) == 0 {
				continue
			}
			matched := false
			for _, want := range allowed {
				matched = matched || target["kind"] == want
			}
			if !matched {
				return empty, ErrBlocked
			}
		}
	}
	if err = previewSourceRightsTx(ctx, tx, scope, prepared, resolved); err != nil {
		return empty, err
	}
	policy, err := store.ReadRoutePolicyTx(ctx, tx, scope)
	if err != nil {
		return empty, err
	}
	if requiresModel {
		if err = store.CheckNewRouteAdmissionTx(ctx, tx, scope, MockConnection, MockModel); err != nil {
			return empty, ErrBlocked
		}
	}
	findings, _ := definitionFindingsPolicy(prepared, kind, false, resolved, policy)
	mappings, routeHash, err := freezeRoutesWithPolicy(prepared, resolved, policy)
	if err != nil {
		return empty, ErrBlocked
	}
	for _, mapping := range mappings {
		if err = store.CheckNewRouteAdmissionTx(ctx, tx, scope, string(mapping.ConnectionRef), string(mapping.ModelID)); err != nil {
			return empty, ErrBlocked
		}
	}
	return store.PackagePreviewInspection{Input: prepared, Findings: findings, RouteHash: routeHash}, nil
}

func previewSourceRightsTx(ctx context.Context, tx store.Tx, scope domain.ProductScope, input generated.DraftInput, resolved ResolvedDefinitions) error {
	local, err := objects(input)
	if err != nil {
		return ErrBlocked
	}
	catalog := map[string]map[string]map[string]any{"": local}
	owners := map[string]string{}
	for _, dep := range input.DependencyLocks {
		release := string(dep.ReleaseID)
		if catalog[release] != nil {
			continue
		}
		if err = tx.RequireLock(Key(scope, "content_release", release)); err != nil {
			return err
		}
		var raw []byte
		err = tx.QueryRowContext(ctx, "SELECT c.document FROM content_releases c JOIN releases r ON r.tenant_id=c.tenant_id AND r.product_id=c.product_id AND r.release_id=c.release_id WHERE c.tenant_id=? AND c.product_id=? AND c.release_id=? AND r.available=1", scope.TenantID, scope.ProductID, release).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrBlocked
		}
		if err != nil {
			return err
		}
		var value generated.Release
		if generated.Decode("Release", raw, &value) != nil {
			return ErrBlocked
		}
		catalog[release], err = objects(value.Input)
		if err != nil {
			return ErrBlocked
		}
		owners[release] = string(value.PublishedBySubjectID)
	}
	var clock time.Time
	if err = tx.QueryRowContext(ctx, "SELECT UTC_TIMESTAMP(6)").Scan(&clock); err != nil {
		return err
	}
	visited := map[string]bool{}
	usedLocks := map[string]bool{}
	var visit func(string, string, int) error
	visit = func(release, id string, depth int) error {
		key := release + "/" + id
		if visited[key] {
			return nil
		}
		if depth > 64 || len(visited) >= 10000 {
			return ErrBlocked
		}
		node := catalog[release][id]
		if err := candidateRights(node, clock); err != nil {
			return ErrBlocked
		}
		copyAllowed := false
		for _, use := range node["rights"].(map[string]any)["allowed_uses"].([]any) {
			copyAllowed = copyAllowed || use == "COPY"
		}
		if !copyAllowed {
			return ErrBlocked
		}
		if release != "" && node["audience_policy"] == "PRODUCT_EDITORS" && owners[release] != scope.SubjectID {
			return ErrBlocked
		}
		visited[key] = true
		if node["kind"] == "ASSET" {
			if err := store.CheckOperatorTx(ctx, tx, scope, "asset.read"); err != nil {
				return err
			}
			assetID := fmt.Sprint(node["asset_id"])
			if err := tx.RequireLock(Key(scope, "asset", assetID)); err != nil {
				return err
			}
			var raw []byte
			var state, owner string
			err := tx.QueryRowContext(ctx, "SELECT state,owner_subject_id,document FROM assets WHERE tenant_id=? AND product_id=? AND asset_id=?", scope.TenantID, scope.ProductID, assetID).Scan(&state, &owner, &raw)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrBlocked
			}
			if err != nil {
				return err
			}
			var file generated.FileRecord
			if generated.Decode("FileRecord", raw, &file) != nil || state != "READY" || file.State != "READY" || file.Scope != Scope(scope) || string(file.AssetID) != assetID || string(file.OwnerSubjectID) != owner || file.Sha256 != node["asset_sha256"] || string(file.SizeBytes) != fmt.Sprint(node["size_bytes"]) || file.MediaType != node["media_type"] {
				return ErrBlocked
			}
			if file.AudiencePolicy != "PRODUCT_READERS" && (file.AudiencePolicy != "PRODUCT_EDITORS" || owner != scope.SubjectID) {
				return ErrBlocked
			}
			if err = candidateUse(file.Rights, clock); err != nil {
				return ErrBlocked
			}
			canCopy := false
			for _, use := range file.Rights.AllowedUses {
				canCopy = canCopy || use == "COPY"
			}
			if !canCopy {
				return ErrBlocked
			}
			available := map[string]bool{}
			for _, use := range file.Rights.AllowedUses {
				available[use] = true
			}
			for _, use := range node["rights"].(map[string]any)["allowed_uses"].([]any) {
				if !available[use.(string)] {
					return ErrBlocked
				}
			}
		}
		for _, ref := range previewSourceEdges(node) {
			next := release
			if ref["kind"] == "PINNED_DEFINITION" {
				k := referenceKey(ref)
				if resolved[k] == nil {
					return ErrBlocked
				}
				usedLocks[k] = true
				next = fmt.Sprint(ref["release_id"])
			}
			if err := visit(next, fmt.Sprint(ref["object_id"]), depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	ids := []string{}
	for id := range local {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err = visit("", id, 0); err != nil {
			return err
		}
	}
	for _, ref := range previewBindingRefs(input) {
		if ref["kind"] == "PINNED_DEFINITION" {
			usedLocks[referenceKey(ref)] = true
		}
		release := ""
		if ref["kind"] == "PINNED_DEFINITION" {
			release = fmt.Sprint(ref["release_id"])
		}
		if err = visit(release, fmt.Sprint(ref["object_id"]), 0); err != nil {
			return err
		}
	}
	// Every explicit transitive lock must actually be consumed by the selected
	// candidate closure, so an unrelated release cannot be probed through preview.
	for _, dep := range input.DependencyLocks {
		if !usedLocks[referenceKey(pinReference(dep))] {
			return ErrBlocked
		}
	}
	return nil
}

// Unlike model-input traversal, local preview verifies every declared source,
// including display-only media and theme references.
func previewSourceEdges(node map[string]any) []map[string]any {
	out := []map[string]any{}
	var walk func(any)
	walk = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			if v["kind"] == "LOCAL_DRAFT" || v["kind"] == "PINNED_DEFINITION" {
				out = append(out, v)
				return
			}
			keys := []string{}
			for key := range v {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				walk(v[key])
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(node)
	return out
}

func previewBindingRefs(input generated.DraftInput) []map[string]any {
	out := []map[string]any{}
	read := func(raw json.RawMessage) {
		var ref map[string]any
		if json.Unmarshal(raw, &ref) == nil {
			out = append(out, ref)
		}
	}
	for _, binding := range input.ModuleAgentBindings {
		read(binding.RoleRef)
	}
	for _, binding := range input.SkillBindings {
		read(binding.SkillRef)
	}
	return out
}
