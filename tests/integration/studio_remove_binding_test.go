package integration

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"testing"

	"loreviva.local/loreviva/internal/generated"
)

func TestStudioRemoveBindingExactOwningCommand(t *testing.T) {
	for _, kind := range []string{"MODULE_BINDING", "SKILL_BINDING"} {
		t.Run(kind, func(t *testing.T) {
			r, id, _ := candidateHTTPFixture(t)
			attachOnlineTextSkill(t, r, id, "GenerationMaterial")
			ctx, scope := runtimeContext(t, r), r.Auth.Scope()
			d, e := r.Content.GetDraft(ctx, scope, id, "")
			if e != nil {
				t.Fatal(e)
			}
			// An optional second binding makes exact selection observable without
			// removing the world's required performer or the referenced definition.
			target := "online-text-binding"
			if kind == "MODULE_BINDING" {
				b := d.Input.ModuleAgentBindings[0]
				b.BindingID = "optional-removal"
				b.Enabled = false
				d.Input.ModuleAgentBindings = append(d.Input.ModuleAgentBindings, b)
				target = string(b.BindingID)
			} else {
				b := d.Input.SkillBindings[0]
				b.BindingID = "retained-skill-binding"
				b.Enabled = false
				d.Input.SkillBindings = append(d.Input.SkillBindings, b)
			}
			n, _ := strconv.ParseUint(string(d.DraftRevision), 10, 64)
			d, e = r.Content.SaveDraft(ctx, scope, id, d.Input, n, "binding-setup")
			if e != nil {
				t.Fatal(e)
			}
			n, _ = strconv.ParseUint(string(d.DraftRevision), 10, 64)
			report, e := r.Content.Impact(ctx, scope, id, n, "REMOVE_BINDING", "binding-report", target)
			if e != nil || len(report.BlockingFindings) != 0 || len(report.SelectedTargets) != 1 || report.SelectedTargets[0].Kind != kind {
				t.Fatal(report, e)
			}
			if _, e = r.Store.DB.Exec("UPDATE content_checks SET document=JSON_SET(CONVERT(document USING utf8mb4),'$.expires_at','2000-01-01T00:00:00Z') WHERE report_id=?", report.ReportID); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Content.Command(ctx, scope, id, n, "REMOVE_BINDING", string(report.ReportID), "", "", "expired-binding-command"); e == nil {
				t.Fatal("expired impact report executed")
			}
			report, e = r.Content.Impact(ctx, scope, id, n, "REMOVE_BINDING", "fresh-binding-report", target)
			if e != nil {
				t.Fatal(e)
			}
			// Real HTTP uses content.edit even when delete/archive privileges are absent.
			if _, e = r.Store.DB.Exec("DELETE FROM operator_actions WHERE action IN ('content.delete','content.archive')"); e != nil {
				t.Fatal(e)
			}
			if _, e = r.Store.DB.Exec("UPDATE studio_operator_grants SET enabled=0 WHERE action IN ('content.delete','content.archive')"); e != nil {
				t.Fatal(e)
			}
			body, _ := json.Marshal(map[string]any{"product_id": scope.ProductID, "expected_draft_revision": d.DraftRevision, "action": "REMOVE_BINDING", "impact_report_id": report.ReportID})
			request := func(key string, payload []byte) *httptest.ResponseRecorder {
				q := httptest.NewRequest("POST", "/world/v1/admin/content-drafts/"+id+"/commands", bytes.NewReader(payload))
				q.Header.Set("Authorization", "Bearer "+runtimeToken)
				q.Header.Set("Content-Type", "application/json")
				q.Header.Set("Idempotency-Key", key)
				w := httptest.NewRecorder()
				r.Handler().ServeHTTP(w, q)
				return w
			}
			r.Store.BeforeCommit = func(ctx context.Context, tx *sql.Tx) error {
				if _, e := tx.ExecContext(ctx, "DELETE FROM operator_actions WHERE credential_id='owner' AND action='content.edit'"); e != nil {
					return e
				}
				_, e := tx.ExecContext(ctx, "UPDATE studio_operator_grants SET enabled=0 WHERE credential_id='owner' AND action='content.edit'")
				return e
			}
			denied := request("binding-revoked-inside-tx", body)
			r.Store.BeforeCommit = nil
			if denied.Code != 403 {
				t.Fatal("owning transaction did not recheck current edit authority", denied.Code, denied.Body.String())
			}
			unchanged, e := r.Content.GetDraft(ctx, scope, id, "")
			if e != nil || unchanged.ContentHash != d.ContentHash || unchanged.DraftRevision != d.DraftRevision {
				t.Fatal("denied command did not roll back", e)
			}
			w := request("binding-remove", body)
			if w.Code != 200 {
				t.Fatalf("owning REMOVE_BINDING: %d %s", w.Code, w.Body.String())
			}
			var receipt generated.DraftCommandReceipt
			if e = generated.Decode("DraftCommandReceipt", w.Body.Bytes(), &receipt); e != nil {
				t.Fatal(e)
			}
			got, e := r.Content.GetDraft(ctx, scope, id, "")
			if e != nil {
				t.Fatal(e)
			}
			if got.DraftRevision != generated.ExactUnsigned(strconv.FormatUint(n+1, 10)) || receipt.DraftRevision != got.DraftRevision {
				t.Fatal("revision not advanced", got.DraftRevision, receipt)
			}
			expected := d.Input
			if kind == "MODULE_BINDING" {
				expected.ModuleAgentBindings = expected.ModuleAgentBindings[:len(expected.ModuleAgentBindings)-1]
			} else {
				expected.SkillBindings = expected.SkillBindings[1:]
			}
			want, _ := json.Marshal(expected)
			actual, _ := json.Marshal(got.Input)
			if !bytes.Equal(want, actual) {
				t.Fatal("command changed data beyond the exact binding")
			}
			replay := request("binding-remove", body)
			if replay.Code != 200 || !bytes.Equal(replay.Body.Bytes(), w.Body.Bytes()) {
				t.Fatal("original receipt changed", replay.Code, replay.Body.String())
			}
			if stale := request("binding-stale", body); stale.Code != 409 {
				t.Fatal("stale revision accepted", stale.Code, stale.Body.String())
			}
			newer := bytes.Replace(body, []byte(`"expected_draft_revision":"`+string(d.DraftRevision)+`"`), []byte(`"expected_draft_revision":"`+string(got.DraftRevision)+`"`), 1)
			if old := request("binding-old-report", newer); old.Code == 200 {
				t.Fatal("old report accepted at new revision")
			}
		})
	}
}
