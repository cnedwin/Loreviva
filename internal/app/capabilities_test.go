package app

import (
	"encoding/json"
	"os"
	"testing"

	"loreviva.local/loreviva/internal/generated"
)

// The capability endpoint compares its embedded table hash with the generated
// source hash at request time and refuses to answer when they differ. That check
// protects callers, but it fails only when a request arrives: a stale constant
// can therefore ship and be discovered in production. This test fails the build
// instead, at the moment the contract is regenerated without updating the table.
func TestCapabilityTableMatchesGeneratedContract(t *testing.T) {
	if capabilityTableSourceHash != generated.SourceHash {
		t.Fatalf("capability table hash %s does not match generated source hash %s: regenerate contracts and update capabilityTableSourceHash",
			capabilityTableSourceHash, generated.SourceHash)
	}

	var profiles []struct {
		Profile string `json:"profile"`
		OpenAPI string `json:"openapi_sha256"`
	}
	if e := json.Unmarshal([]byte(capabilityProfiles), &profiles); e != nil {
		t.Fatalf("capability profiles are not readable: %v", e)
	}
	raw, e := os.ReadFile("../../contracts/generated/manifest.json")
	if e != nil {
		t.Skip("generated manifest not available in this checkout")
	}
	var manifest struct {
		ProfileHashes map[string]string `json:"profile_hashes"`
	}
	if e = json.Unmarshal(raw, &manifest); e != nil {
		t.Fatal(e)
	}
	if len(manifest.ProfileHashes) != len(profiles) {
		t.Fatalf("published %d profiles but embedded %d", len(manifest.ProfileHashes), len(profiles))
	}
	for _, published := range manifest.ProfileHashes {
		found := false
		for _, embedded := range profiles {
			if embedded.OpenAPI == published {
				found = true
			}
		}
		if !found {
			t.Fatalf("generated profile %s is not published by the capability table", published)
		}
	}
}
