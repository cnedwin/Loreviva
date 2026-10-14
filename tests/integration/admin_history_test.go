package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/harness"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type candidateHistoryReader interface {
	ReadApprovedDraftCandidate(context.Context, domain.ProductScope, string) ([]byte, error)
}

func TestProductCandidateSourceBytesAtOwningBoundaries(t *testing.T) {
	for _, mode := range []string{"INTAKE", "AUTHORIZE", "START_SEND", "CONSUME", "CONSUME_CORRUPT", "VALIDATE", "REPAIR", "EXTRA_AUTHORIZE", "EXTRA_SEND", "COMMIT", "HISTORY_MISSING", "HISTORY_CORRUPT"} {
		t.Run(mode, func(t *testing.T) {
			r, original, asset, _ := historySourceCandidate(t, false)
			ctx := runtimeContext(t, r)
			var storage, draft string
			if e := r.Store.DB.QueryRow("SELECT storage_key FROM assets WHERE asset_id=?", asset).Scan(&storage); e != nil {
				t.Fatal(e)
			}
			if e := r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", original.TaskID).Scan(&draft); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(r.Files.RootPath, original.TenantID, original.ProductID, storage+".blob")
			remove := func() {
				t.Helper()
				var counts string
				query := "SELECT CONCAT((SELECT COUNT(*) FROM attempts),':',(SELECT COUNT(*) FROM reservations),':',(SELECT COUNT(*) FROM commit_receipts),':',(SELECT COUNT(*) FROM harness_candidate_commits),':',(SELECT COUNT(*) FROM service_events))"
				if e := r.Store.DB.QueryRow(query).Scan(&counts); e != nil {
					t.Fatal(e)
				}
				journal := journalBytes(t, r)
				t.Cleanup(func() {
					var after string
					if e := r.Store.DB.QueryRow(query).Scan(&after); e != nil || after != counts {
						t.Error("source denial changed original lifecycle", counts, after, e)
					}
					if !bytes.Equal(journal, journalBytes(t, r)) {
						t.Error("source denial called provider")
					}
				})
				if strings.HasSuffix(mode, "CORRUPT") {
					if e := os.WriteFile(path, []byte("source corrupt"), 0600); e != nil {
						t.Fatal(e)
					}
				} else {
					if e := os.Remove(path); e != nil {
						t.Fatal(e)
					}
				}
			}
			q := generated.CandidateRequest{ProductID: generated.Identity(original.ProductID), ExpectedDraftRevision: "1", Mode: "MOCK", ModuleID: "AUTHORING", ObjectID: "coffee-world", SelectedFields: []string{"description"}, ApprovedCostUnits: "0"}
			if mode == "INTAKE" {
				remove()
				if _, e := r.Store.AcceptDraftCandidate(ctx, original.ProductScope, draft, "physical-source-next", q); e == nil {
					t.Fatal("missing source intake")
				}
				var count int
				if e := r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_admin_candidates").Scan(&count); e != nil || count != 1 {
					t.Fatal("failed intake reserved a task", count, e)
				}
				return
			}
			if strings.HasPrefix(mode, "HISTORY") {
				remove()
				if _, e := r.Files.ReadApprovedDraftCandidate(ctx, original.ProductScope, original.TaskID); e == nil {
					t.Fatal("missing source history")
				}
				return
			}
			accepted, e := r.Store.AcceptDraftCandidate(ctx, original.ProductScope, draft, "physical-source-next", q)
			if e != nil {
				t.Fatal(e)
			}
			claimed, e := r.Store.ClaimRegistered(ctx, "candidate-worker", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "CANDIDATE"}})
			if e != nil || claimed.TaskID != string(accepted.TaskID) {
				t.Fatal(e)
			}
			l := claimed.TaskLease
			d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
			if e != nil {
				t.Fatal(e)
			}
			if mode == "AUTHORIZE" {
				remove()
				if _, e = r.Store.AuthorizeDispatch(ctx, d); e == nil {
					t.Fatal("missing source authorize")
				}
				return
			}
			if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
			if e != nil {
				t.Fatal(e)
			}
			if mode == "START_SEND" {
				remove()
				if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
					t.Fatal("missing source send")
				}
				return
			}
			a, e := r.Store.StartSend(ctx, d, d.AttemptID)
			if e != nil {
				t.Fatal(e)
			}
			if strings.HasPrefix(mode, "CONSUME") {
				remove()
				if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e == nil {
					t.Fatal("missing source Generate")
				}
				return
			}
			r.Worker.Turns.Fault = 0
			if mode == "REPAIR" || strings.HasPrefix(mode, "EXTRA") {
				r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
			}
			if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
				t.Fatal(e)
			}
			if mode == "VALIDATE" {
				remove()
				if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e == nil {
					t.Fatal("missing source validation")
				}
				return
			}
			if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
				t.Fatal(e)
			}
			if mode == "COMMIT" {
				remove()
				if _, e = r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e == nil {
					t.Fatal("missing source business COMMIT")
				}
				return
			}
			if mode == "REPAIR" {
				remove()
				if _, e = r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker"); e == nil {
					t.Fatal("missing source repair approval")
				}
				return
			}
			d, e = r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
			if e != nil {
				t.Fatal(e)
			}
			if mode == "EXTRA_AUTHORIZE" {
				remove()
				if _, e = r.Store.AuthorizeDispatch(ctx, d); e == nil {
					t.Fatal("missing source extra authorize")
				}
				return
			}
			if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
				t.Fatal(e)
			}
			d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
			if e != nil {
				t.Fatal(e)
			}
			remove()
			if _, e = r.Store.StartSend(ctx, d, d.AttemptID); e == nil {
				t.Fatal("missing source extra send")
			}
		})
	}
}

func TestProductCandidateHistoryCurrentInstallationAndStream(t *testing.T) {
	for _, mode := range []string{"STREAM_READ_REVOKED", "STREAM_BODY_REMOVED", "CLOSING", "MISSING_STORE_WRITER"} {
		t.Run(mode, func(t *testing.T) {
			r, l, _ := validCandidateBusiness(t, false)
			ctx := runtimeContext(t, r)
			if _, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e != nil {
				t.Fatal(e)
			}
			opened, e := r.Files.OpenAuthorized(ctx, l.ProductScope, content.Identity(l.TaskID, "candidate-asset"))
			if e != nil {
				t.Fatal(e)
			}
			defer opened.Close()
			switch mode {
			case "STREAM_READ_REVOKED":
				if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.read'"); e != nil {
					t.Fatal(e)
				}
			case "STREAM_BODY_REMOVED":
				var key string
				if e = r.Store.DB.QueryRow("SELECT storage_key FROM harness_candidate_results WHERE task_id=?", l.TaskID).Scan(&key); e != nil {
					t.Fatal(e)
				}
				if e = os.Remove(filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, key+".part")); e != nil {
					t.Fatal(e)
				}
			case "CLOSING":
				if e = r.Local.Coordinator.BeginClose(ctx, "candidate-history-close"); e != nil {
					t.Fatal(e)
				}
			case "MISSING_STORE_WRITER":
				original := r.Store.Guard
				r.Store.Guard = nil
				defer func() { r.Store.Guard = original }()
			}
			if n, e := opened.Read(make([]byte, 1024)); e == nil || n != 0 {
				t.Fatal("stale stream disclosed", n, e)
			}
		})
	}
}

func historySourceCandidate(t *testing.T, pinned bool) (*app.Runtime, domain.TaskLease, string, string) {
	t.Helper()
	ctx := context.Background()
	assetID := ""
	sourceRelease := ""
	r, l := candidateDispatchInputFixture(t, func(r *app.Runtime, s domain.ProductScope, in *generated.DraftInput) {
		ctx = runtimeContext(t, r)
		var ref generated.DefinitionRef
		if pinned {
			var raw []byte
			if e := r.Store.DB.QueryRow("SELECT document FROM content_releases ORDER BY release_id LIMIT 1").Scan(&raw); e != nil {
				t.Fatal(e)
			}
			var release generated.Release
			if e := json.Unmarshal(raw, &release); e != nil {
				t.Fatal(e)
			}
			sourceRelease = string(release.ReleaseID)
			for _, object := range release.Input.Objects {
				var m map[string]any
				json.Unmarshal(object, &m)
				if m["kind"] != "ROLE" || ref != nil {
					continue
				}
				hash, e := content.Hash(m)
				if e != nil {
					t.Fatal(e)
				}
				dep := generated.DependencyLock{ReleaseID: release.ReleaseID, ObjectID: generated.Identity(m["object_id"].(string)), Version: generated.ExactUnsigned(m["version"].(string)), ContentHash: hash}
				in.DependencyLocks = append(in.DependencyLocks, dep)
				ref, _ = json.Marshal(map[string]any{"kind": "PINNED_DEFINITION", "release_id": dep.ReleaseID, "object_id": dep.ObjectID, "version": dep.Version, "content_hash": dep.ContentHash})
			}
		} else {
			body := []byte("private source bytes absent from model input")
			hash := sha256.Sum256(body)
			rights := generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "declared origin", License: "Apache-2.0", AllowedUses: []string{"EDIT"}}
			asset, e := r.Files.Upload(ctx, s, bytes.NewReader(body), generated.AssetUploadMetadata{ProductID: generated.Identity(s.ProductID), Name: "origin.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: generated.ExactUnsigned("44"), ExpectedSha256: hex.EncodeToString(hash[:]), AudiencePolicy: "PRODUCT_EDITORS", Rights: rights}, "history-source-upload")
			if e != nil {
				t.Fatal("real source upload", e)
			}
			assetID = string(asset.AssetID)
			definition := generated.AssetDefinition{ObjectID: "selected-origin", Kind: "ASSET", Name: "Origin", Description: "source private annotation", Tags: []string{}, Rights: rights, AudiencePolicy: "PRODUCT_EDITORS", Version: "1", SourceRefs: []string{}, DependencyRefs: []generated.DefinitionRef{}, AssetID: asset.AssetID, AssetSha256: asset.Sha256, SizeBytes: asset.SizeBytes, MediaType: "text/plain", ClientCapabilities: []generated.Identity{}, Fallback: "BLOCK"}
			second, e := r.Files.Upload(ctx, s, bytes.NewReader(body), generated.AssetUploadMetadata{ProductID: generated.Identity(s.ProductID), Name: "second-origin.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "44", ExpectedSha256: hex.EncodeToString(hash[:]), AudiencePolicy: "PRODUCT_EDITORS", Rights: rights}, "history-second-source-upload")
			if e != nil {
				t.Fatal(e)
			}
			secondDefinition := definition
			secondDefinition.ObjectID = "selected-origin-second"
			secondDefinition.AssetID = second.AssetID
			secondRaw, _ := json.Marshal(secondDefinition)
			in.Objects = append(in.Objects, secondRaw)
			definition.DependencyRefs = []generated.DefinitionRef{json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"selected-origin-second"}`)}
			raw, _ := json.Marshal(definition)
			in.Objects = append(in.Objects, raw)
			ref = json.RawMessage(`{"kind":"LOCAL_DRAFT","object_id":"selected-origin"}`)
		}
		for i, raw := range in.Objects {
			var m map[string]any
			json.Unmarshal(raw, &m)
			if m["object_id"] == string(in.RootObjectID) {
				m["dependency_refs"] = []generated.DefinitionRef{ref}
				in.Objects[i], _ = json.Marshal(m)
			}
		}
	})
	d, e := r.Store.PrepareDraftCandidateDispatch(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	a, e := r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	r.Worker.Turns.Fault = harness.MockMalformedOriginalResult
	if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	d, e = r.Store.PrepareDraftCandidateRecovery(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.AuthorizeDispatch(ctx, d); e != nil {
		t.Fatal(e)
	}
	d, e = r.Store.PrepareDraftCandidateSend(ctx, l, "candidate-worker")
	if e != nil {
		t.Fatal(e)
	}
	a, e = r.Store.StartSend(ctx, d, d.AttemptID)
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Worker.Turns.GenerateDraftCandidate(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.ObserveOriginalReply(ctx, a.OriginalSend()); e != nil {
		t.Fatal(e)
	}
	if e = r.Store.ValidateDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
		t.Fatal("source-backed history", e)
	}
	if !pinned {
		view, e := r.Store.InspectApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID)
		if e != nil || len(view.Sources) != 2 || view.Sources[0].Storage >= view.Sources[1].Storage {
			t.Fatal("fixed multi-source order", e)
		}
	}
	return r, l, assetID, sourceRelease
}

func TestProductCandidateHistoryOriginalSourceClosure(t *testing.T) {
	for _, mode := range []string{"SWAP_FUTURE_REFS", "ASSET_USE", "ASSET_EXPIRED", "PINNED_UNAVAILABLE"} {
		t.Run(mode, func(t *testing.T) {
			r, l, asset, release := historySourceCandidate(t, mode == "PINNED_UNAVAILABLE")
			ctx := runtimeContext(t, r)
			if mode == "PINNED_UNAVAILABLE" {
				if _, e := r.Store.DB.Exec("UPDATE releases SET available=0 WHERE release_id=?", release); e != nil {
					t.Fatal(e)
				}
			} else if mode == "ASSET_EXPIRED" {
				if _, e := r.Store.DB.Exec("UPDATE assets SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.rights.expires_at','2000-01-01T00:00:00Z') WHERE asset_id=?", asset); e != nil {
					t.Fatal(e)
				}
			} else if mode == "ASSET_USE" {
				if _, e := r.Store.DB.Exec("UPDATE assets SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.rights.allowed_uses',JSON_ARRAY()) WHERE asset_id=?", asset); e != nil {
					t.Fatal(e)
				}
			} else {
				var draft string
				if e := r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
					t.Fatal(e)
				}
				d, e := r.Content.GetDraft(ctx, l.ProductScope, draft, "")
				if e != nil {
					t.Fatal(e)
				}
				for i, raw := range d.Input.Objects {
					var m map[string]any
					json.Unmarshal(raw, &m)
					if m["object_id"] == string(d.Input.RootObjectID) {
						m["dependency_refs"] = []any{}
						d.Input.Objects[i], _ = json.Marshal(m)
					}
				}
				if _, e = r.Content.SaveDraft(ctx, l.ProductScope, draft, d.Input, 1, "history-future-refs"); e != nil {
					t.Fatal(e)
				}
				if _, e = r.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
					t.Fatal("new refs replaced original closure", e)
				}
				for i, raw := range d.Input.Objects {
					var m map[string]any
					json.Unmarshal(raw, &m)
					if m["object_id"] == "selected-origin" {
						m["rights"].(map[string]any)["allowed_uses"] = []any{}
						d.Input.Objects[i], _ = json.Marshal(m)
					}
				}
				if _, e = r.Content.SaveDraft(ctx, l.ProductScope, draft, d.Input, 2, "history-old-source-revoke"); e != nil {
					t.Fatal(e)
				}
			}
			if _, e := r.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID); e == nil {
				t.Fatal("old source withdrawal ignored", mode)
			}
		})
	}
}

func TestProductCandidatePartialSourceCleanupRetiresBothBodies(t *testing.T) {
	r, l, asset, _ := historySourceCandidate(t, false)
	ctx := runtimeContext(t, r)
	if _, e := r.Store.DB.Exec("UPDATE assets SET state='DELETED' WHERE asset_id=?", asset); e != nil {
		t.Fatal(e)
	}
	var upper uint64
	var cutoff time.Time
	if e := r.Store.DB.QueryRow("SELECT MAX(sequence_id),DATE_ADD(UTC_TIMESTAMP(6),INTERVAL 1 DAY) FROM file_staging").Scan(&upper, &cutoff); e != nil {
		t.Fatal(e)
	}
	page, e := r.Files.CleanupPage(ctx, l.ProductScope, cutoff, 0, upper)
	if e != nil || page.Removed != 2 {
		t.Fatal("partial source deletion did not retire original and repair", page, e)
	}
	var intents int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM harness_candidate_results WHERE task_id=? AND delete_intent=1", l.TaskID).Scan(&intents); e != nil || intents != 2 {
		t.Fatal(intents, e)
	}
	if receipt, e := r.Store.QueryDraftCandidateCommit(ctx, l.ProductScope, l.TaskID); e != nil || !receipt.Committed {
		t.Fatal("cleanup altered business fact", e)
	}
}

func TestProductCandidateHistoryReadsActualCommittedBytes(t *testing.T) {
	r, l, d := validCandidateBusiness(t, true)
	ctx := runtimeContext(t, r)
	original, e := r.Files.ReadPendingCandidateResult(ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e != nil {
		t.Fatal(e)
	}
	reader, ok := any(r.Files).(candidateHistoryReader)
	if !ok {
		t.Fatal("real S55 committed candidate reader unavailable")
	}
	body, e := reader.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID)
	if e != nil || !bytes.Equal(body, original) {
		t.Fatal("actual historical bytes", e)
	}
	var draft string
	if e = r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
		t.Fatal(e)
	}
	current, e := r.Content.GetDraft(ctx, l.ProductScope, draft, "")
	if e != nil {
		t.Fatal(e)
	}
	for i, raw := range current.Input.Objects {
		var object map[string]any
		if json.Unmarshal(raw, &object) != nil {
			t.Fatal("fixture object")
		}
		if object["object_id"] == string(current.Input.RootObjectID) {
			object["description"] = "ordinary newer local editor text"
			object["generation"] = map[string]any{"mode": "NONE", "required_capabilities": []any{}, "allowed_external_uses": []any{"NONE"}}
			current.Input.Objects[i], e = json.Marshal(object)
			if e != nil {
				t.Fatal(e)
			}
		}
	}
	if _, e = r.Content.SaveDraft(ctx, l.ProductScope, draft, current.Input, 1, "candidate-history-newer-save"); e != nil {
		t.Fatal("actual new revision", e)
	}
	if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.assist'"); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE subject_governance SET can_execute=0 WHERE tenant_id=? AND product_id=? AND subject_id=?", l.TenantID, l.ProductID, l.SubjectID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE tasks SET lease_expires_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", l.TaskID); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.DB.Exec("UPDATE harness_envelopes SET accepted_at=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 181 SECOND),execute_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 61 SECOND),settle_by=DATE_SUB(UTC_TIMESTAMP(6),INTERVAL 1 SECOND) WHERE task_id=?", l.TaskID); e != nil {
		t.Fatal(e)
	}
	var beforeReceipts, beforeSteps, beforeCalls int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),e.steps_used,t.calls_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&beforeReceipts, &beforeSteps, &beforeCalls); e != nil {
		t.Fatal(e)
	}
	body, e = reader.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID)
	if e != nil || !bytes.Equal(body, original) {
		t.Fatal("current read/use independent from old generation gates", e)
	}
	asset, e := r.Files.Get(ctx, l.ProductScope, content.Identity(l.TaskID, "candidate-asset"))
	if e != nil {
		t.Fatal("fixed asset metadata", e)
	}
	opened, e := r.Files.OpenAuthorized(ctx, l.ProductScope, string(asset.AssetID))
	if e != nil {
		t.Fatal("fixed asset bytes", e)
	}
	actual, e := io.ReadAll(opened)
	opened.Close()
	if e != nil || !bytes.Equal(actual, original) {
		t.Fatal("asset uses actual part bytes", e)
	}
	var receipts, steps, calls int
	if e = r.Store.DB.QueryRow("SELECT (SELECT COUNT(*) FROM commit_receipts),e.steps_used,t.calls_used FROM tasks t JOIN harness_envelopes e USING(tenant_id,product_id,task_id) WHERE t.task_id=?", l.TaskID).Scan(&receipts, &steps, &calls); e != nil || receipts != beforeReceipts || steps != beforeSteps || calls != beforeCalls {
		t.Fatal("read mutated lifecycle", receipts, steps, calls, e)
	}
}

func TestProductCandidateHistoryCurrentBoundaries(t *testing.T) {
	for _, mode := range []string{"READ", "ASSET_READ", "CREDENTIAL", "SUBJECT", "OWNER", "PRODUCT", "SOURCE_USE", "LICENSE", "EXPIRY", "MISSING_RECEIPT", "BAD_RECEIPT", "VALIDATION", "SEND", "LINEAGE", "OLD_NO_PROOF", "ORIGINAL_MISSING", "EXTRA_MISSING", "ORIGINAL_CORRUPT", "EXTRA_CORRUPT"} {
		t.Run(mode, func(t *testing.T) {
			r, l, _ := validCandidateBusiness(t, true)
			ctx := runtimeContext(t, r)
			if _, e := r.Store.CommitDraftCandidate(ctx, l, "candidate-worker"); e != nil {
				t.Fatal(e)
			}
			if _, e := r.Files.ReadApprovedDraftCandidate(ctx, l.ProductScope, l.TaskID); e != nil {
				t.Fatal("positive baseline", e)
			}
			scope := l.ProductScope
			var q string
			args := []any{}
			switch mode {
			case "READ":
				q = "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.read'"
			case "ASSET_READ":
				q = "DELETE FROM operator_actions WHERE credential_id='owner' AND action='asset.read'"
			case "CREDENTIAL":
				q = "UPDATE operator_credentials SET enabled=0 WHERE credential_id='owner'"
			case "SUBJECT":
				q = "UPDATE subject_governance SET enabled=0 WHERE subject_id='creator'"
			case "OWNER":
				scope.SubjectID = "another-owner"
			case "PRODUCT":
				scope.ProductID = "another-product"
			case "MISSING_RECEIPT":
				q = "DELETE FROM commit_receipts WHERE commit_key=?"
				args = []any{content.Identity(l.TaskID, "candidate-commit")}
			case "BAD_RECEIPT":
				q = "UPDATE commit_receipts SET payload_hash=UNHEX(REPEAT('ab',32)) WHERE commit_key=?"
				args = []any{content.Identity(l.TaskID, "candidate-commit")}
			case "VALIDATION":
				q = "UPDATE harness_original_validations SET metadata=JSON_SET(metadata,'$.BodyHash',REPEAT('a',64)) WHERE task_id=?"
				args = []any{l.TaskID}
			case "SEND":
				q = "DELETE FROM commit_receipts WHERE phase='SEND' AND task_id=?"
				args = []any{l.TaskID}
			case "LINEAGE":
				q = "UPDATE harness_candidate_results SET delete_intent=1 WHERE task_id=?"
				args = []any{l.TaskID}
			case "OLD_NO_PROOF":
				q = "UPDATE harness_candidate_commits SET document=JSON_REMOVE(document,'$.Binding.Sources','$.Binding.SourceHash') WHERE task_id=?"
				args = []any{l.TaskID}
			case "SOURCE_USE", "LICENSE", "EXPIRY":
				var draft string
				if e := r.Store.DB.QueryRow("SELECT draft_id FROM harness_admin_candidates WHERE task_id=?", l.TaskID).Scan(&draft); e != nil {
					t.Fatal(e)
				}
				d, e := r.Content.GetDraft(ctx, l.ProductScope, draft, "")
				if e != nil {
					t.Fatal(e)
				}
				for i, raw := range d.Input.Objects {
					var m map[string]any
					json.Unmarshal(raw, &m)
					if m["object_id"] != string(d.Input.RootObjectID) {
						continue
					}
					rights := m["rights"].(map[string]any)
					switch mode {
					case "SOURCE_USE":
						rights["allowed_uses"] = []any{}
					case "LICENSE":
						rights["license"] = " "
					case "EXPIRY":
						rights["expires_at"] = "2000-01-01T00:00:00Z"
					}
					d.Input.Objects[i], _ = json.Marshal(m)
				}
				if _, e = r.Content.SaveDraft(ctx, l.ProductScope, draft, d.Input, 1, "history-revoke-source"); e != nil {
					t.Fatal("actual rights save", e)
				}
			default:
				slot := 0
				if strings.HasPrefix(mode, "EXTRA") {
					slot = 1
				}
				var key string
				if e := r.Store.DB.QueryRow("SELECT r.storage_key FROM harness_candidate_results r JOIN attempts a USING(tenant_id,product_id,task_id,attempt_id) WHERE r.task_id=? AND a.slot=?", l.TaskID, slot).Scan(&key); e != nil {
					t.Fatal(e)
				}
				path := filepath.Join(r.Files.RootPath, l.TenantID, l.ProductID, key+".part")
				if strings.HasSuffix(mode, "MISSING") {
					if e := os.Remove(path); e != nil {
						t.Fatal(e)
					}
				} else {
					if e := os.WriteFile(path, []byte("corrupt actual result"), 0600); e != nil {
						t.Fatal(e)
					}
				}
			}
			if q != "" {
				if _, e := r.Store.DB.Exec(q, args...); e != nil {
					t.Fatal("fixture restriction", e)
				}
			}
			if _, e := r.Files.ReadApprovedDraftCandidate(ctx, scope, l.TaskID); e == nil {
				t.Fatal("terminal read bypassed", mode)
			}
			if opened, e := r.Files.OpenAuthorized(ctx, scope, content.Identity(l.TaskID, "candidate-asset")); e == nil {
				opened.Close()
				t.Fatal("ordinary asset fallback bypassed", mode)
			}
			var state string
			if e := r.Store.DB.QueryRow("SELECT state FROM tasks WHERE task_id=?", l.TaskID).Scan(&state); e != nil || state != "COMMITTED" {
				t.Fatal("history denial erased COMMIT", state, e)
			}
		})
	}
}
