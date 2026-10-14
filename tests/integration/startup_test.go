package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/auth"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/maintenance"
	store "loreviva.local/loreviva/internal/store/mysql"
	"loreviva.local/loreviva/tests/integration/dbfixture"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestM1Startup(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	f, e := files.New(s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	token := "synthetic-private-test-credential-32bytes"
	a := auth.New(s, scope, "owner", token)
	r := app.NewRuntime(s, f, a)
	if r.Readiness(ctx) == nil {
		t.Fatal("unmigrated startup ready")
	}
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	if r.Readiness(ctx) == nil {
		t.Fatal("missing governance/auth startup ready")
	}
	if e = maintenance.BootstrapSynthetic(ctx, s, scope, "owner", token); e != nil {
		t.Fatal(e)
	}
	if e = r.Readiness(ctx); e != nil {
		t.Fatal("fresh synthetic readiness", e)
	}
	if _, e = s.DB.Exec("DELETE FROM governance_watermark"); e != nil {
		t.Fatal(e)
	}
	if r.Readiness(ctx) == nil {
		t.Fatal("restored installation without watermark ready")
	}
	if e = maintenance.BootstrapSynthetic(ctx, s, scope, "owner", token); e == nil {
		t.Fatal("fresh zero gate recreated over restored subjects")
	}
	if _, e = s.DB.Exec("INSERT INTO governance_watermark VALUES(1,0,0,1)"); e != nil {
		t.Fatal(e)
	}
	emptyAuth := auth.New(s, scope, "owner", "")
	if emptyAuth.Check(ctx) == nil {
		t.Fatal("empty configured credential authenticated")
	}
	registryRuntime := app.NewRuntime(s, f, emptyAuth)
	if e = registryRuntime.Readiness(ctx); e != nil {
		t.Fatal("actual registered credential did not make registry ready", e)
	}
	if _, e = s.DB.Exec("UPDATE operator_credentials SET enabled=0"); e != nil {
		t.Fatal(e)
	}
	if registryRuntime.Readiness(ctx) == nil {
		t.Fatal("registry with all credentials disabled ready")
	}
	if _, e = s.DB.Exec("UPDATE operator_credentials SET enabled=1 WHERE credential_id='owner'"); e != nil {
		t.Fatal(e)
	}
	if e = registryRuntime.Readiness(ctx); e != nil {
		t.Fatal("restored registered credential unready", e)
	}
	f.Close()
	if r.Readiness(ctx) == nil {
		t.Fatal("invalid files ready")
	}
}
func TestRegisteredOnlyWorkerDoesNotClaimFutureTurn(t *testing.T) {
	r, scope, _ := runtimeFixture(t)
	s, db := r.Store, r.Store.DB
	ctx := context.Background()
	if _, e := db.Exec("INSERT INTO tasks(tenant_id,product_id,task_id,owner_subject_id,scope_kind,state,next_run_at,task_kind) VALUES(?,?,'future',?,'PRODUCT','ACCEPTED',UTC_TIMESTAMP(6),'ADMIN')", scope.TenantID, scope.ProductID, scope.SubjectID); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ClaimRegistered(ctx, "m1", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "FILE_CLEANUP"}}); !errors.Is(e, store.ErrNoTask) {
		t.Fatal("unsupported future job claimed", e)
	}
	owner := runtimeContext(t, r)
	const secret = "synthetic-cleanup-only-credential-at-least32"
	if e := maintenance.RegisterLocalCredential(owner, s, scope, "cleanup-only", secret); e != nil {
		t.Fatal(e)
	}
	grant := generated.StudioMemberCommandRequest(`{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"kind":"SET_SCOPE_GRANT","expected_revision":"0","credential_id":"cleanup-only","subject_id":"creator","grant":{"scope":{"scope_type":"PRODUCT","tenant_id":"demo-local","product_id":"coffee"},"action":"asset.upload","use":"PRODUCT_ADMIN","effect":"ALLOW","enabled":true,"expires_at":null}}`)
	if _, e := s.ApplyMember(owner, "cleanup-only-grant", grant); e != nil {
		t.Fatal(e)
	}
	caller, _, e := r.Auth.Authenticate(ctx, "Bearer "+secret)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.CheckOperator(caller, scope, "content.edit"); e == nil {
		t.Fatal("cleanup-only credential acquired unrelated content authority")
	}
	receipt, e := app.QueueCleanup(caller, s, scope, "cleanup")
	if e != nil {
		t.Fatal(e)
	}
	var provenance string
	var issuer sql.NullString
	if e = db.QueryRow("SELECT issuer_provenance,issuer_credential_id FROM registered_jobs WHERE task_id=? AND operation='FILE_CLEANUP'", receipt.TaskID).Scan(&provenance, &issuer); e != nil || provenance != "PROGRAM_OWNED" || issuer.Valid {
		t.Fatal("cleanup program provenance", provenance, issuer.Valid, e)
	}
	key := content.Identity(scope.TenantID, scope.ProductID, scope.SubjectID, "queueFileCleanup", "cleanup")
	intake, e := s.QueryCommitScoped(ctx, scope, nil, scope.SubjectID, key)
	if e != nil || !intake.Committed || intake.Key != key {
		t.Fatal("exact cleanup intake receipt missing", e)
	}
	lease, e := s.ClaimRegistered(ctx, "m1", time.Now(), time.Minute, []store.RegisteredOperation{{Kind: "ADMIN", Operation: "FILE_CLEANUP"}})
	if e != nil || lease.TaskID != string(receipt.TaskID) || lease.Operation != "FILE_CLEANUP" {
		t.Fatal("supported job not claimed", lease, e)
	}
}
func TestActualAdminHTTPOriginalWireScopeAndUnavailableJobs(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	token := "private-synthetic-test-operator-token"
	if e := maintenance.BootstrapSynthetic(ctx, s, scope, "owner", token); e != nil {
		t.Fatal(e)
	}
	f, e := files.New(s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	a := auth.New(s, scope, "owner", token)
	r := app.NewRuntime(s, f, a)
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	userCtx, _, e := a.Authenticate(ctx, "Bearer "+token)
	if e != nil {
		t.Fatal(e)
	}
	seed, e := content.New(s).SeedCoffee(userCtx, scope, "coffee-v1")
	if e != nil {
		t.Fatal(e)
	}
	call := func(method, path, body, key, credential string) int {
		t.Helper()
		req, e := http.NewRequest(method, server.URL+path, strings.NewReader(body))
		if e != nil {
			t.Fatal(e)
		}
		if credential != "" {
			req.Header.Set("Authorization", "Bearer "+credential)
		}
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		req.Header.Set("Content-Type", "application/json")
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	base := "/world/v1/admin/content-drafts/" + seed.DraftID
	if code := call("GET", base+"?product_id=coffee", "", "", ""); code != 401 {
		t.Fatal("unauthenticated", code)
	}
	if code := call("GET", base+"?product_id=other", "", "", token); code != 404 {
		t.Fatal("scope enumeration", code)
	}
	if code := call("GET", base+"?product_id=coffee", "", "", token); code != 200 {
		t.Fatal("authorized query", code)
	}
	body := `{"product_id":"coffee","expected_draft_revision":"1","mode":"MOCK","fixture":"COFFEE","inputs":[],"max_cost_units":"0","assertions":[]}`
	if code := call("POST", base+"/rehearsals", body, "rehearse", token); code != 503 {
		t.Fatal("fake unsupported202", code)
	}
	var count int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&count); e != nil || count != 0 {
		t.Fatal("unsupported durable intake", count, e)
	}
	if code := call("POST", base+"/publish-checks", `{"product_id":"coffee","product_id":"coffee","expected_draft_revision":"1","requested_support_labels":["MOCK_DEMO"]}`, "dup", token); code != 400 {
		t.Fatal("duplicate key accepted", code)
	}
	h := sha256.Sum256([]byte(token))
	var credentialHash []byte
	if e = s.DB.QueryRow("SELECT token_sha256 FROM operator_credentials").Scan(&credentialHash); e != nil || hex.EncodeToString(credentialHash) != hex.EncodeToString(h[:]) {
		t.Fatal("server owned credential", e)
	}
	var docs []byte
	d, e := content.New(s).GetDraft(userCtx, scope, seed.DraftID, "")
	if e != nil {
		t.Fatal(e)
	}
	docs, _ = json.Marshal(d)
	if bytes.Contains(docs, []byte(token)) {
		t.Fatal("secret content leak")
	}
}

func TestMultipartRejectedBeforeDurableIntakeAndPrivateProxy(t *testing.T) {
	ctx := context.Background()
	s := store.New(dbfixture.Open(t))
	if e := s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	scope := domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}
	token := "private-synthetic-test-operator-token"
	if e := maintenance.BootstrapSynthetic(ctx, s, scope, "owner", token); e != nil {
		t.Fatal(e)
	}
	f, e := files.New(s, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r := app.NewRuntime(s, f, auth.New(s, scope, "owner", token))
	server := httptest.NewServer(r.Handler())
	defer server.Close()
	body := []byte("private synthetic file")
	h := sha256.Sum256(body)
	metadata := generated.AssetUploadMetadata{ProductID: "coffee", Name: "note.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "22", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_EDITORS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "test", License: "Apache-2.0", AllowedUses: []string{"EDIT", "COPY", "PUBLISH"}}}
	raw, _ := json.Marshal(metadata)
	upload := func(extra bool, key string) (int, generated.FileRecord) {
		var b bytes.Buffer
		mw := multipart.NewWriter(&b)
		p, _ := mw.CreateFormField("metadata")
		p.Write(raw)
		p, _ = mw.CreateFormFile("file", "note.txt")
		p.Write(body)
		if extra {
			p, _ = mw.CreateFormField("extra")
			p.Write([]byte("forbidden"))
		}
		mw.Close()
		q, _ := http.NewRequest("POST", server.URL+"/world/v1/admin/assets", &b)
		q.Header.Set("Content-Type", mw.FormDataContentType())
		q.Header.Set("Authorization", "Bearer "+token)
		q.Header.Set("Idempotency-Key", key)
		res, e := http.DefaultClient.Do(q)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		var record generated.FileRecord
		json.NewDecoder(res.Body).Decode(&record)
		return res.StatusCode, record
	}
	code, _ := upload(true, "extra-part")
	if code != 400 {
		t.Fatal("extra multipart status", code)
	}
	var n int
	if e = s.DB.QueryRow("SELECT COUNT(*) FROM assets").Scan(&n); e != nil || n != 0 {
		t.Fatal("invalid request committed file", n, e)
	}
	code, record := upload(false, "valid-part")
	if code != 201 || record.State != "READY" {
		t.Fatal("valid upload", code, record)
	}
	for _, credential := range []string{"", token} {
		q, _ := http.NewRequest("GET", server.URL+"/world/v1/assets/"+string(record.AssetID)+"/content?product_id=coffee", nil)
		if credential != "" {
			q.Header.Set("Authorization", "Bearer "+credential)
		}
		res, e := http.DefaultClient.Do(q)
		if e != nil {
			t.Fatal(e)
		}
		got, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if credential == "" {
			if res.StatusCode != 404 || bytes.Contains(got, body) {
				t.Fatal("unauthorized private bytes", res.StatusCode)
			}
		} else if res.StatusCode != 200 || !bytes.Equal(got, body) || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("private proxy", res.StatusCode)
		}
	}
}
