package integration

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	example "loreviva.local/loreviva/examples/skills/source-aware-reply"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	world "loreviva.local/loreviva/internal/runtime"
	"loreviva.local/loreviva/internal/wepkg"
)

func skillManifest(t *testing.T, raw []byte) wepkg.Manifest {
	t.Helper()
	z, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range z.File {
		if f.Name == "manifest.json" {
			r, e := f.Open()
			if e != nil {
				t.Fatal(e)
			}
			b, e := io.ReadAll(io.LimitReader(r, wepkg.DefaultLimits.ManifestBytes+1))
			r.Close()
			m, findings := wepkg.ParseManifest(b, wepkg.DefaultLimits)
			if e != nil || len(findings) > 0 {
				t.Fatal(e, findings)
			}
			return m
		}
	}
	t.Fatal("actual exported manifest missing")
	return wepkg.Manifest{}
}

func skillArchive(t *testing.T) ([]byte, string) {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	path := filepath.Join(filepath.Dir(file), "../../examples/skills/source-aware-reply/source-aware-reply.wepkg")
	raw, e := os.ReadFile(path)
	built, _, build := example.Build()
	if e != nil || build != nil || !bytes.Equal(raw, built) {
		t.Fatal("actual repository Skill archive differs", e, build)
	}
	return raw, path
}
func skillMapping(t *testing.T, f packageFixture, archive []byte) map[string]any {
	t.Helper()
	discovered := previewPost(t, f, archive, map[string]any{"stage": "DISCOVER"}, f.token)
	var d map[string]any
	if discovered.Code != 200 || json.Unmarshal(discovered.Body.Bytes(), &d) != nil || d["status"] != "NEEDS_MAPPING" {
		t.Fatalf("sample discover%d %s", discovered.Code, discovered.Body.String())
	}
	requirements := d["requirements"].(map[string]any)
	rights, models := []any{}, []any{}
	caps := map[string]string{}
	for _, v := range requirements["objects"].([]any) {
		rights = append(rights, map[string]any{"object_id": v.(map[string]any)["object_id"], "allowed_uses": transferUses})
	}
	for _, v := range requirements["capabilities"].([]any) {
		c := v.(map[string]any)
		id := c["capability_id"].(string)
		// A capability can be consumed by generation and the parent Skill.
		// Match both declared uses, just as the creator UI does.
		uses := map[string]bool{}
		for _, use := range c["usages"].([]any) {
			uses[use.(string)] = true
		}
		if c["family"] == "MODEL" && uses["GENERATION"] {
			models = append(models, map[string]any{"capability_id": id, "settings": map[string]any{"mode": "OVERRIDE", "connection_ref": "mock-local", "model_id": "synthetic-text-v1", "required_capabilities": []string{"TEXT"}, "allowed_external_uses": []string{"NONE"}}})
		}
		if uses["SKILL"] || c["family"] == "PRESENTATION" || (c["family"] == "MODEL" && !uses["GENERATION"]) {
			if id == "text" {
				caps[id] = "TEXT"
			} else if id == "sign_custody" {
				caps[id] = "SIGN_CUSTODY"
			} else {
				t.Fatal("unmapped actual capability", id)
			}
		}
	}
	if len(requirements["files"].([]any)) != 0 {
		t.Fatal("static method example unexpectedly requires a file asset")
	}
	return map[string]any{"stage": "RESOLVE", "entrypoint": d["entrypoint"], "namespace": d["namespace"], "dependency_mappings": []any{}, "additional_locks": []any{}, "model_mappings": models, "local_capabilities": caps, "asset_mappings": []any{}, "rights": rights, "rights_attested": true}
}
func skillImport(t *testing.T, r *app.Runtime, archive []byte, key string) generated.ContentDraft {
	t.Helper()
	f := transferFixture(t, r)
	mapping := skillMapping(t, f, archive)
	resolved := previewPost(t, f, archive, mapping, f.token)
	var d map[string]any
	if resolved.Code != 200 || json.Unmarshal(resolved.Body.Bytes(), &d) != nil || d["status"] != "RESOLVED" {
		t.Fatalf("actual Skill local resolution%d %s", resolved.Code, resolved.Body.String())
	}
	out := importedResult(t, importPost(t, f, archive, map[string]any{"mapping": mapping, "expected_preview_sha256": d["preview_sha256"]}, key, context.Background()))
	return importDraft(t, f, out.Receipt.DraftID)
}
func skillDefinition(t *testing.T, input generated.DraftInput) (map[string]any, string) {
	t.Helper()
	for _, raw := range input.Objects {
		var o map[string]any
		json.Unmarshal(raw, &o)
		if o["kind"] == "SKILL" {
			hash, e := content.Hash(json.RawMessage(raw))
			if e != nil {
				t.Fatal(e)
			}
			return o, hash
		}
	}
	t.Fatal("actual imported Skill missing")
	return nil, ""
}
func skillInstall(t *testing.T, r *app.Runtime, archive []byte, key string) generated.Release {
	t.Helper()
	d := skillImport(t, r, archive, key)
	if d.State != "ISOLATED_IMPORT" {
		t.Fatal("source file bypassed isolation")
	}
	task := transferRehearse(t, r, d, key+"-rehearsal", []string{"ISOLATED", "RIGHTS", "DUTIES", "SIGN_CUSTODY"})
	check := transferCheck(t, r, d, key+"-check")
	approved, e := r.Content.ApprovePackageDraft(runtimeContext(t, r), transferScope(), string(d.DraftID), transferApproval(d, check, task), key+"-approve")
	if e != nil {
		t.Fatal("actual imported Skill world approval", e)
	}
	return transferPublish(t, r, approved, key+"-release")
}
func skillParentTurn(t *testing.T, r *app.Runtime, release generated.Release, key string) string {
	t.Helper()
	ctx := runtimeContext(t, r)
	instance := runtimeControl(t, r, createRuntimeWorld(t, r, string(release.ReleaseID), key+"-world"), "START")
	scope := domain.Scope{ProductScope: transferScope(), WorldInstanceID: instance.Scope.WorldInstanceID}
	var raw []byte
	if e := r.Store.DB.QueryRow("SELECT initialization FROM runtime_instances WHERE world_instance_id=?", scope.WorldInstanceID).Scan(&raw); e != nil {
		t.Fatal(e)
	}
	var init world.Initialization
	if e := json.Unmarshal(raw, &init); e != nil || len(init.Actors) == 0 {
		t.Fatal("actual imported initialization", e)
	}
	actor := init.Actors[0].ID
	quote, e := r.Turns.QuoteTurn(ctx, scope, generated.QuoteRequest{Scope: instance.Scope, ServiceKind: "TURN_TEXT", PrimaryActorID: &actor, InputUpperBound: "16000", OutputUpperBound: "1000", PurchaseResponsibility: "MOCK"}, key+"-quote")
	if e != nil {
		t.Fatal(e)
	}
	turn, e := r.Turns.SubmitTurn(ctx, scope, generated.TurnRequest{QuoteID: quote.QuoteID, ExpectedVersions: generated.ExpectedVersions{WorldRevision: instance.Versions.WorldRevision, ControlVersion: instance.Versions.ControlVersion}, PrimaryActorID: actor, RequestedAudience: "PRIVATE_RELATIONSHIP", Input: json.RawMessage(`{"kind":"TEXT","text":"Hello"}`)}, key+"-turn")
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Once(ctx); e != nil {
		t.Fatal("actual imported parent worker", e)
	}
	var calls, invocations, steps int
	if e = r.Store.DB.QueryRow("SELECT calls_used,e.steps_used,(SELECT COUNT(*) FROM harness_skill_invocations WHERE task_id=t.task_id) FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", turn.TurnID).Scan(&calls, &steps, &invocations); e != nil || calls != 1 || invocations != 1 || steps != 11 {
		t.Fatal("imported Skill bought a second call or new envelope", calls, steps, invocations, e)
	}
	var stored, hash []byte
	if e = r.Store.DB.QueryRow("SELECT definition,definition_hash FROM harness_skill_invocations WHERE task_id=?", turn.TurnID).Scan(&stored, &hash); e != nil {
		t.Fatal(e)
	}
	var binding domain.TurnSkill
	if json.Unmarshal(stored, &binding) != nil {
		t.Fatal("stored actual Skill invocation")
	}
	_, want := skillDefinition(t, release.Input)
	if binding.DefinitionHash != want || binding.SourceRelease != string(release.ReleaseID) || binding.Capability != "TEXT" || binding.InputSchema != "GenerationMaterial" || binding.OutputSchema != "DispatchAuthorization" || len(hash) != 32 {
		t.Fatalf("actual imported definition proof%+v %s", binding, hex.EncodeToString(hash))
	}
	view, e := r.Turns.GetTurn(ctx, scope, string(turn.TurnID))
	if e != nil || len(view.ApprovedPackets) != 1 {
		t.Fatal("actual parent result", e)
	}
	return string(turn.TurnID)
}

func TestSkillSharingRepositoryRoundtripAndParentBudget(t *testing.T) {
	archive, _ := skillArchive(t)
	first, _, old := runtimeFixtureWithConsent(t, true)
	original, e := first.Content.GetRelease(runtimeContext(t, first), transferScope(), old)
	if e != nil {
		t.Fatal(e)
	}
	oldBytes := mustPreviewJSON(t, original)
	release := skillInstall(t, first, archive, "skill-first")
	definition, _ := skillDefinition(t, release.Input)
	var source map[string]any
	json.Unmarshal(example.Source(), &source)
	var originalSkill map[string]any
	for _, v := range source["objects"].([]any) {
		o := v.(map[string]any)
		if o["kind"] == "SKILL" {
			originalSkill = o
		}
	}
	if definition["description"] != originalSkill["description"] || !bytes.Equal(mustPreviewJSON(t, definition["synthetic_examples"]), mustPreviewJSON(t, originalSkill["synthetic_examples"])) || definition["object_id"] == originalSkill["object_id"] {
		t.Fatal("method/examples or local ID mapping changed incorrectly")
	}
	parent := skillParentTurn(t, first, release, "skill-first")
	request := generated.PackageExportRequest{ProductID: "coffee", ReleaseID: release.ReleaseID, PackageID: "loreviva.shared-source-aware-reply", PackageVersion: "1.0.0", AuthorDisplayName: "Synthetic local creator", RightsAttested: true}
	receipt, e := first.Content.ExportPackage(runtimeContext(t, first), transferScope(), request, "skill-export")
	if e != nil {
		t.Fatal(e)
	}
	_, exported, e := first.Store.ReadPackageExport(runtimeContext(t, first), transferScope(), string(receipt.ExportID))
	if e != nil {
		t.Fatal(e)
	}
	report := wepkg.Check(exported, wepkg.DefaultLimits)
	if report.Verdict != "PASS" {
		t.Fatal("real Skill export rejected", report.Findings)
	}
	manifest := skillManifest(t, exported)
	if manifest.Compatibility.MinimumRuntimeContract != "1.11.0" {
		t.Fatal("Skill requirement minimum omitted")
	}
	second, _, _ := runtimeFixtureWithConsent(t, true)
	installed := skillInstall(t, second, exported, "skill-second")
	again, _ := skillDefinition(t, installed.Input)
	if again["description"] != definition["description"] || again["object_id"] == definition["object_id"] || installed.ReleaseID == release.ReleaseID {
		t.Fatal("second installation changed method or reused local IDs")
	}
	skillParentTurn(t, second, installed, "skill-second")
	current, e := first.Content.GetRelease(runtimeContext(t, first), transferScope(), old)
	if e != nil || !bytes.Equal(oldBytes, mustPreviewJSON(t, current)) {
		t.Fatal("sharing changed original immutable release", e)
	}
	var calls int
	if e = first.Store.DB.QueryRow("SELECT calls_used FROM tasks WHERE task_id=?", parent).Scan(&calls); e != nil || calls != 1 {
		t.Fatal("sharing reset existing parent budget", e)
	}
}

func TestSkillSharingIncompatibleAdapterCannotProcure(t *testing.T) {
	r, _, _ := runtimeFixtureWithConsent(t, true)
	_, manifest, e := example.Build()
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"wrong-input", "program-disguise"} {
		t.Run(mode, func(t *testing.T) {
			var data map[string]any
			json.Unmarshal(example.Source(), &data)
			for _, v := range data["objects"].([]any) {
				o := v.(map[string]any)
				if o["kind"] == "SKILL" {
					if mode == "wrong-input" {
						o["input_schema_id"] = "DraftInput"
					} else {
						o["implementation_kind"] = "PROGRAM"
					}
				}
			}
			m := manifest
			m.PackageID += "." + mode
			body, _ := json.Marshal(data)
			archive, _, e := wepkg.ExportBundle(m, body, nil)
			if e != nil {
				t.Fatal("closed declarative fixture must reach actual adapter admission", e)
			}
			d := skillImport(t, r, archive, "skill-invalid-"+mode)
			var slot string
			for _, v := range d.Input.Objects {
				var o map[string]any
				json.Unmarshal(v, &o)
				if o["kind"] == "WORLD" {
					slot = o["primary_actor_slot"].(string)
				}
			}
			var req generated.RehearsalRequest
			generated.Decode("RehearsalRequest", mustPreviewJSON(t, map[string]any{"product_id": "coffee", "expected_draft_revision": d.DraftRevision, "mode": "MOCK", "fixture": "COFFEE", "inputs": []any{map[string]any{"actor_slot": slot, "text": ""}}, "max_cost_units": "0", "assertions": []string{"ISOLATED"}}), &req)
			before := transferCount(t, r, "tasks")
			if _, e = r.Store.AcceptDraftRehearsal(runtimeContext(t, r), transferScope(), string(d.DraftID), "invalid-adapter-"+mode, req); e == nil {
				t.Fatal("incompatible adapter admitted")
			}
			if after := transferCount(t, r, "tasks"); after != before {
				t.Fatal("refused adapter procured work")
			}
		})
	}
}

func TestSkillSharingCreatorBrowser(t *testing.T) {
	if os.Getenv("LOREVIVA_SKILL_SHARING_BROWSER") != "1" {
		t.Skip("explicit owned Skill sharing browser fixture required")
	}
	_, archive := skillArchive(t)
	r, _, _ := runtimeFixtureWithConsent(t, true)
	f := transferFixture(t, r)
	base := f.server.Config.Handler
	f.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.Method == "POST" && strings.HasSuffix(q.URL.Path, "/rehearsals") {
			out := httptest.NewRecorder()
			base.ServeHTTP(out, q)
			if out.Code == 202 {
				if e := r.Worker.Once(runtimeContext(t, r)); e != nil {
					t.Error(e)
				}
			}
			for k, v := range out.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(out.Code)
			w.Write(out.Body.Bytes())
			return
		}
		base.ServeHTTP(w, q)
	})
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Getenv("LOREVIVA_TEST_NODE"), "scripts/skill-sharing-browser.mjs")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "LOREVIVA_SKILL_BASE="+f.server.URL, "LOREVIVA_SKILL_BEARER="+runtimeToken, "LOREVIVA_SKILL_ARCHIVE="+archive)
	raw, e := cmd.CombinedOutput()
	t.Log(string(raw))
	if e != nil {
		t.Fatal("actual Skill creator browser", e)
	}
	if transferCount(t, r, "content_package_imports") != 1 || transferCount(t, r, "content_package_adoptions") != 1 || transferCount(t, r, "content_package_exports") != 1 {
		t.Fatal("actual sample workflow side effects differ")
	}
	var invocations int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_skill_invocations WHERE capability='TEXT'").Scan(&invocations); e != nil || invocations != 1 {
		t.Fatal("browser imported Skill was not actually used in its parent rehearsal", invocations, e)
	}
}
