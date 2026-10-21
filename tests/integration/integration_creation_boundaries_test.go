package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"loreviva.local/loreviva/internal/app"
	"loreviva.local/loreviva/internal/content"
	"loreviva.local/loreviva/internal/domain"
	"loreviva.local/loreviva/internal/generated"
	store "loreviva.local/loreviva/internal/store/mysql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Each denial first gets real publication, registration and SERVICE authority.
// This matrix does not mint CLIENT sessions or pretend that metadata is proof.
func TestIntegrationCreateAdmissionBoundaries(t *testing.T) {
	for _, mode := range []string{"zero", "two", "shared", "product", "player", "unpublished", "customer_disabled", "mapping_disabled"} {
		t.Run(mode, func(t *testing.T) {
			r, op, ctx, req, _ := integrationCreationFixture(t)
			switch mode {
			case "zero":
				req.InitialParticipants = []string{}
			case "two":
				req.InitialParticipants = []string{"s66-player", "creator"}
			case "shared":
				req.Mode = "SHARED"
			case "product":
				req.ProductID = "other-product"
			case "player":
				req.InitialParticipants = []string{"creator"}
			case "unpublished":
				req.ReleaseID = "unpublished-release"
			case "customer_disabled", "mapping_disabled":
				cmd := domain.RegistryCommand{ProductID: "coffee", ExpectedRevision: 1}
				if mode == "customer_disabled" {
					cmd.Kind = "DISABLE_CUSTOMER"
					cmd.TargetID = "s66-customer"
				} else {
					cmd.Kind = "DISABLE_PLAYER_MAPPING"
					cmd.TargetID = "s66-player"
					cmd.QualifiedTarget = &domain.RegistryQualifiedTarget{CustomerID: "s66-customer"}
				}
				if _, e := r.Store.ApplyIntegrationRegistry(op, "s66-current-disable", cmd); e != nil {
					t.Fatal("real current disable prerequisite", e)
				}
			}
			_, e := r.World.CreateIntegrationInstance(ctx, "s66-boundary", req)
			id := content.Identity("demo-local", "coffee", "s66-service", "createInstance", "s66-boundary")
			if e == nil || integrationWorldRows(t, r.Store, id) != 0 {
				t.Fatal("denial/atomicity", mode, e)
			}
		})
	}
}

func TestIntegrationCreateQualifiedCustomerAndGrantSet(t *testing.T) {
	r, op, ctx, req, cmd := integrationCreationFixture(t)
	var now time.Time
	if e := r.Store.DB.QueryRow("SELECT UTC_TIMESTAMP(6)").Scan(&now); e != nil {
		t.Fatal(e)
	}
	secret := "synthetic-s66-other-customer-service-32"
	file := filepath.Join(t.TempDir(), "other-key")
	if e := os.WriteFile(file, []byte(secret), 0600); e != nil {
		t.Fatal(e)
	}
	other := domain.RegistryCommand{Kind: "REGISTER_CUSTOMER", ProductID: "coffee", TargetID: "other-customer", Customer: &domain.RegisterCustomer{CustomerID: "other-customer", ServiceSubjectID: "other-service", ConsumerID: "other-consumer", SecretHandleRef: file, ExpiresAt: now.Add(time.Hour)}}
	if _, e := r.Store.ApplyIntegrationRegistry(op, "s66-other-customer", other); e != nil {
		t.Fatal(e)
	}
	foreign, _, e := r.Store.AuthenticateIntegration(context.Background(), "Bearer "+secret, domain.IntegrationService)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = r.World.CreateIntegrationInstance(foreign, "s66-other-create", req); e == nil {
		t.Fatal("another real customer reused player mapping")
	}
	if integrationWorldRows(t, r.Store, content.Identity("demo-local", "coffee", "other-service", "createInstance", "s66-other-create")) != 0 {
		t.Fatal("foreign partial world")
	}
	p, e := r.Store.PrepareIntegrationCreation(ctx, "s66-grant-race", req)
	if e != nil {
		t.Fatal(e)
	}
	v := *cmd.Player
	v.EntitlementID = "s66-late-grant"
	cmd.Player = &v
	if _, e = r.Store.ApplyIntegrationRegistry(op, "s66-late-grant", cmd); e != nil {
		t.Fatal(e)
	}
	if _, e = r.Store.RunIntegrationCreation(ctx, p); !errors.Is(e, store.ErrConflict) || integrationWorldRows(t, r.Store, p.WorldID()) != 0 {
		t.Fatal("late ambiguous set not rechecked under ownership", e)
	}
	for _, mode := range []string{"expiry", "release", "entitlement", "customer", "external", "player"} {
		c := cmd
		x := *cmd.Player
		c.Player = &x
		switch mode {
		case "expiry":
			x.ExpiresAt = x.ExpiresAt.Add(-time.Minute)
		case "release":
			x.ReleaseID = "different-release"
		case "entitlement":
			x.EntitlementID = "different-entitlement"
		case "customer":
			x.CustomerID = "other-customer"
		case "external":
			x.ExternalIDBytes = []byte("different-full-external")
		case "player":
			x.PlayerSubjectID = "different-player"
			c.TargetID = x.PlayerSubjectID
		}
		if _, e = r.Store.ApplyIntegrationRegistry(op, "s66-late-grant", c); !errors.Is(e, store.ErrConflict) {
			t.Fatal("same-key payload not conflict", mode, e)
		}
	}
}

func TestIntegrationCreateAdditionalSourceBoundaries(t *testing.T) {
	for _, mode := range []string{"asset_id", "asset_hash", "extract_report", "document", "world_origin", "character_origin", "origin_cycle", "origin_depth", "origin_wrong_kind"} {
		t.Run(mode, func(t *testing.T) {
			r, _, ctx, req, _ := integrationCreationFixturePrepared(t, func(r *app.Runtime, op context.Context, in *generated.DraftInput) {
				createPolicyInput(t, "owned")(in)
				var asset generated.FileRecord
				if mode == "asset_id" || mode == "asset_hash" {
					body := []byte("Synthetic S66 source bytes")
					h := sha256.Sum256(body)
					var e error
					asset, e = r.Files.Upload(op, domain.ProductScope{TenantID: "demo-local", ProductID: "coffee", SubjectID: "creator"}, bytes.NewReader(body), generated.AssetUploadMetadata{ProductID: "coffee", Name: "s66.txt", Purpose: "KNOWLEDGE", DeclaredType: "text/plain", ExpectedSize: generated.ExactUnsigned(fmt.Sprint(len(body))), ExpectedSha256: hex.EncodeToString(h[:]), AudiencePolicy: "PRODUCT_READERS", Rights: generated.SourceRights{OriginKind: "SYNTHETIC", SourceLabel: "actual local synthetic source", License: "CC0-1.0", AllowedUses: []string{"EDIT", "COPY", "PUBLISH", "REDISTRIBUTE"}}}, "s66-real-asset")
					if e != nil {
						t.Fatal("real FileRecord prerequisite", e)
					}
				}
				for i, raw := range in.Objects {
					var o map[string]any
					_ = json.Unmarshal(raw, &o)
					if (mode == "world_origin" && o["kind"] == "WORLD") || (mode == "character_origin" && o["kind"] == "CHARACTER") {
						o["origin_ref"] = map[string]any{"kind": "LOCAL_DRAFT", "object_id": "s66-note"}
					}
					if o["object_id"] == "s66-note" {
						switch mode {
						case "asset_id":
							o["source_asset_id"] = asset.AssetID
						case "asset_hash":
							o["source_asset_hash"] = asset.Sha256
						case "extract_report":
							o["extraction_report_id"] = "unimplemented-extraction-owner"
						case "document":
							o["knowledge_type"] = "DOCUMENT_EXTRACT"
						case "origin_cycle":
							o["origin_ref"] = map[string]any{"kind": "LOCAL_DRAFT", "object_id": "s66-note"}
						case "origin_wrong_kind":
							o["origin_ref"] = map[string]any{"kind": "LOCAL_DRAFT", "object_id": in.RootObjectID}
						case "origin_depth":
							o["origin_ref"] = map[string]any{"kind": "LOCAL_DRAFT", "object_id": "s66-chain-0"}
							for n := 0; n < 8; n++ {
								p := map[string]any{}
								for k, v := range o {
									p[k] = v
								}
								p["object_id"] = fmt.Sprintf("s66-chain-%d", n)
								delete(p, "origin_ref")
								if n < 7 {
									p["origin_ref"] = map[string]any{"kind": "LOCAL_DRAFT", "object_id": fmt.Sprintf("s66-chain-%d", n+1)}
								}
								b, _ := json.Marshal(p)
								in.Objects = append(in.Objects, b)
							}
						}
					}
					in.Objects[i], _ = json.Marshal(o)
				}
			}, time.Hour)
			_, e := r.World.CreateIntegrationInstance(ctx, "s66-source-boundary", req)
			var fields *content.FieldErrors
			if !errors.As(e, &fields) {
				t.Fatal("not actual CREATE initial-source denial", e)
			}
			if integrationWorldRows(t, r.Store, content.Identity("demo-local", "coffee", "s66-service", "createInstance", "s66-source-boundary")) != 0 {
				t.Fatal("partial world")
			}
		})
	}
}

func TestIntegrationRegistryAppendOriginalKeyBoundaries(t *testing.T) {
	r, op, _, _, cmd := integrationCreationFixture(t)
	v := *cmd.Player
	v.EntitlementID = "s66-short"
	v.ExpiresAt = v.ExpiresAt.Add(-30 * time.Minute)
	cmd.Player = &v
	receipt, e := r.Store.ApplyIntegrationRegistry(op, "s66-short-key", cmd)
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"expiry", "external", "player", "customer", "extra_action", "duplicate"} {
		c := cmd
		p := *cmd.Player
		c.Player = &p
		key := "s66-negative-" + mode
		switch mode {
		case "expiry":
			p.ExpiresAt = p.ExpiresAt.Add(-time.Minute)
			key = "s66-short-key"
		case "external":
			p.ExternalIDBytes = []byte("different-external")
		case "player":
			p.PlayerSubjectID = "creator"
			c.TargetID = "creator"
		case "customer":
			p.CustomerID = "unknown-customer"
		case "extra_action":
			p.Actions = []string{"READ", "MEMORY_VIEW"}
		}
		if _, e = r.Store.ApplyIntegrationRegistry(op, key, c); e == nil {
			t.Fatal("append accepted", mode)
		}
		if mode == "expiry" && !errors.Is(e, store.ErrConflict) {
			t.Fatal("same key changed original expiry", e)
		}
	}
	read, e := r.Store.ReadIntegrationRegistryReceipt(op, "s66-short-key")
	if e != nil || !read.RegistrationExpiry.EntitlementExpiresAt.Equal(*receipt.RegistrationExpiry.EntitlementExpiresAt) {
		t.Fatal("original receipt altered", e)
	}
	var n int
	if e = r.Store.DB.QueryRow("SELECT COUNT(*) FROM integration_entitlements WHERE customer_id='s66-customer'").Scan(&n); e != nil || n != 2 {
		t.Fatal("rejected append wrote rows", n, e)
	}
}
