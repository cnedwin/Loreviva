package content_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"loreviva.local/loreviva/internal/files"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/tests/integration/testprincipal"
	"testing"
)

func TestFix1PersistedUploadRightsConstrainPublish(t *testing.T) {
	for _, limitation := range []string{"EDIT_ONLY", "EXPIRED", "EXPIRING", "NO_LICENSE", "EDITORS", "ALLOWED", "REVOKED_AFTER_CHECK"} {
		t.Run(limitation, func(t *testing.T) {
			s, scope, ctx := fixture(t)
			f, e := files.New(s.Store, t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			body := []byte("synthetic source")
			h := sha256.Sum256(body)
			rights := generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "original", License: "Apache-2.0", AllowedUses: []string{"EDIT", "PUBLISH"}}
			m := generated.AssetUploadMetadata{ProductID: "coffee", Name: "source.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "16", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_READERS", Rights: rights}
			switch limitation {
			case "EDIT_ONLY":
				m.Rights.AllowedUses = []string{"EDIT"}
			case "EXPIRED":
				v := "2000-01-01T00:00:00Z"
				m.Rights.ExpiresAt = &v
			case "EXPIRING":
				v := "2100-01-01T00:00:00Z"
				m.Rights.ExpiresAt = &v
			case "NO_LICENSE":
				m.Rights.License = " "
			case "EDITORS":
				m.AudiencePolicy = "PRODUCT_EDITORS"
			}
			r, e := f.Upload(ctx, scope, bytes.NewReader(body), m, "source-upload")
			if e != nil {
				t.Fatal(e)
			}
			asset := generated.AssetDefinition{ObjectID: "source-asset", Kind: "ASSET", Name: "Permissive wrapper", Description: "", Tags: []string{}, Rights: rights, AudiencePolicy: "PRODUCT_READERS", Version: "1", SourceRefs: []string{}, DependencyRefs: []generated.DefinitionRef{}, AssetID: r.AssetID, AssetSha256: r.Sha256, SizeBytes: r.SizeBytes, MediaType: "text/plain", ClientCapabilities: []generated.Identity{}, Fallback: "BLOCK"}
			raw, _ := json.Marshal(asset)
			in := coffee(t)
			in.Objects = append(in.Objects, raw)
			d, e := s.Create(ctx, scope, "WORLD", "", in, "source-create")
			if e != nil {
				t.Fatal(e)
			}
			check, e := s.CheckPublish(ctx, scope, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "source-check")
			allowed := limitation == "ALLOWED" || limitation == "REVOKED_AFTER_CHECK"
			if e != nil || check.Passed != allowed {
				t.Errorf("persisted rights limitation=%s passed=%v error=%v findings=%v", limitation, check.Passed, e, check.Findings)
			}
			if limitation == "REVOKED_AFTER_CHECK" {
				r.Rights.AllowedUses = []string{"EDIT"}
				b, _ := json.Marshal(r)
				if _, e = s.Store.DB.Exec("UPDATE assets SET document=? WHERE asset_id=?", b, r.AssetID); e != nil {
					t.Fatal(e)
				}
			}
			_, e = s.Publish(ctx, scope, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "source-publish")
			if (e == nil) != (limitation == "ALLOWED") {
				t.Errorf("persisted rights publication limitation=%s error=%v", limitation, e)
			}
			var count int
			if e = s.Store.DB.QueryRow("SELECT COUNT(*) FROM content_releases").Scan(&count); e != nil {
				t.Fatal(e)
			}
			if limitation != "ALLOWED" && count != 0 {
				t.Errorf("rejected rights mutated releases=%d", count)
			}
			current, e := s.GetDraft(ctx, scope, string(d.DraftID), "")
			if e != nil || current.DraftRevision != "1" {
				t.Fatal("publish mutated draft", e)
			}
		})
	}
}

func TestFix1PersistedAudienceEligibilityForDifferentCreator(t *testing.T) {
	for _, audience := range []string{"PRODUCT_EDITORS", "PRODUCT_READERS"} {
		t.Run(audience, func(t *testing.T) {
			s, scope, ctx := fixture(t)
			f, e := files.New(s.Store, t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			body := []byte("synthetic source")
			h := sha256.Sum256(body)
			rights := generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "original owner", License: "Apache-2.0", AllowedUses: []string{"EDIT", "PUBLISH"}}
			m := generated.AssetUploadMetadata{ProductID: "coffee", Name: "owner-source.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: "16", ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: audience, Rights: rights}
			r, e := f.Upload(ctx, scope, bytes.NewReader(body), m, "owner-source")
			if e != nil {
				t.Fatal(e)
			}
			other := scope
			other.SubjectID = "different-creator"
			if _, e = s.Store.DB.Exec("INSERT INTO subject_governance(tenant_id,product_id,subject_id,permission_version,privacy_epoch,view_epoch,enabled) VALUES('demo-local','coffee','different-creator',1,1,1,1)"); e != nil {
				t.Fatal(e)
			}
			asset := generated.AssetDefinition{ObjectID: "source-asset", Kind: "ASSET", Name: "Same audience wrapper", Description: "", Tags: []string{}, Rights: rights, AudiencePolicy: audience, Version: "1", SourceRefs: []string{}, DependencyRefs: []generated.DefinitionRef{}, AssetID: r.AssetID, AssetSha256: r.Sha256, SizeBytes: r.SizeBytes, MediaType: "text/plain", ClientCapabilities: []generated.Identity{}, Fallback: "BLOCK"}
			raw, _ := json.Marshal(asset)
			in := coffee(t)
			in.Objects = append(in.Objects, raw)
			otherCtx := testprincipal.Register(t, s.Store, ctx, other)
			d, e := s.Create(otherCtx, other, "WORLD", "", in, "different-owner-create")
			if e != nil {
				t.Fatal(e)
			}
			check, e := s.CheckPublish(otherCtx, other, string(d.DraftID), 1, []string{"MOCK_DEMO"}, "different-owner-check")
			if e != nil || check.Passed != (audience == "PRODUCT_READERS") {
				t.Fatalf("actual source audience eligibility=%s findings=%v error=%v", audience, check.Findings, e)
			}
			_, e = s.Publish(otherCtx, other, string(d.DraftID), 1, string(check.ReportID), []string{"MOCK_DEMO"}, "different-owner-publish")
			if (e == nil) != (audience == "PRODUCT_READERS") {
				t.Fatalf("same-label different-creator publication=%s error=%v", audience, e)
			}
			var count int
			if e = s.Store.DB.QueryRow("SELECT COUNT(*) FROM content_releases").Scan(&count); e != nil {
				t.Fatal(e)
			}
			if audience == "PRODUCT_EDITORS" && count != 0 {
				t.Fatal("editor-owned source leaked through equal wrapper label")
			}
		})
	}
}
