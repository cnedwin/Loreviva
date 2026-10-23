package app

import (
	"context"
	"encoding/json"
	"loreviva.local/loreviva/internal/generated"
	"loreviva.local/loreviva/internal/wire"
	"net/http"
	"reflect"
	"regexp"
)

// These closed routes/profile fingerprints are projected from the same fixed
// source as generated.IntegrationCapabilities. A descriptor never grants caller
// authority: actual mount and current owning composition are checked separately.
const capabilityTableSourceHash = "8b47e97040db2fa8b3d9ef4ad65459b649c580b59fbfef74ca4a3208e88420ce"
const capabilityProfiles = `[{"profile":"public_v1","selector_header":"Loreviva-Client-Contract","default":true,"openapi_ref":"/contracts/profiles/public_v1.openapi.json","openapi_sha256":"338e8d86cda919f495170f3cdf0744ead0c7f1d6adc11a094174687539924479","projections":[]},{"profile":"integration_r0_v1","selector_header":"Loreviva-Client-Contract","default":false,"openapi_ref":"/contracts/profiles/integration_r0_v1.openapi.json","openapi_sha256":"af191848ce68c4f283d72a0a8c2e01550deef180cec1065d4708d3ab7cebc96f","projections":[{"operation_id":"getTurn","status":"200","media_type":"application/json","schema_name":"IntegrationTurnView","parameter":"NONE","value":"DEFAULT"},{"operation_id":"getEvents","status":"200","media_type":"application/json","schema_name":"IntegrationEventPage","parameter":"NONE","value":"DEFAULT"},{"operation_id":"streamEvents","status":"200","media_type":"text/event-stream","schema_name":"IntegrationSSEUpdate","parameter":"NONE","value":"DEFAULT","sse_data_schema":"IntegrationSSEUpdate"},{"operation_id":"listMemories","status":"200","media_type":"application/json","schema_name":"PlayerMemoryMetadataPage","parameter":"include","value":"DEFAULT"},{"operation_id":"getMemory","status":"200","media_type":"application/json","schema_name":"PlayerMemoryMetadata","parameter":"include","value":"DEFAULT"}]}]`

type capabilityRoute struct{ method, path, group string }

var capabilityRoutes = map[string]capabilityRoute{
	"preflightContentPackage":           {"POST", "/world/v1/admin/content-packages/preflight", "content"},
	"getContentPackageCapabilities":     {"GET", "/world/v1/admin/content-packages/capabilities", "content"},
	"cancelAdminTask":                   {"POST", "/world/v1/admin/tasks/{task_id}/cancel", "task"},
	"cancelTask":                        {"POST", "/world/v1/tasks/{task_id}/cancel", "task"},
	"cancelTurn":                        {"POST", "/world/v1/turns/{turn_id}/cancel", "task"},
	"checkDraftForPublish":              {"POST", "/world/v1/admin/content-drafts/{draft_id}/publish-checks", "content"},
	"checkDraftImpact":                  {"POST", "/world/v1/admin/content-drafts/{draft_id}/impact-checks", "content"},
	"commandAdminMembers":               {"POST", "/world/v1/admin/settings/members/commands", "settings"},
	"commandAdminSelfConsent":           {"POST", "/world/v1/admin/settings/self-consent/commands", "consent"},
	"commandAdminSettings":              {"POST", "/world/v1/admin/settings/commands", "settings"},
	"commandContentDraft":               {"POST", "/world/v1/admin/content-drafts/{draft_id}/commands", "content"},
	"commandPlayerSelfConsent":          {"POST", "/world/v1/self-consent/commands", "consent"},
	"controlAdminInstance":              {"POST", "/world/v1/admin/instances/{instance_id}/controls", "world"},
	"controlInstance":                   {"POST", "/world/v1/instances/{instance_id}/controls", "world"},
	"createAdminInstance":               {"POST", "/world/v1/admin/instances", "world"},
	"createContentDraft":                {"POST", "/world/v1/admin/content-drafts", "content"},
	"createDraftCandidate":              {"POST", "/world/v1/admin/content-drafts/{draft_id}/candidates", "candidate"},
	"createInstance":                    {"POST", "/world/v1/instances", "world"},
	"createPaymentIntent":               {"POST", "/world/v1/billing/payment-intents", "payment"},
	"createQuote":                       {"POST", "/world/v1/quotes", "store"},
	"exchangeClientSession":             {"POST", "/world/v1/client-sessions", "store"},
	"executeInstanceMigration":          {"POST", "/world/v1/admin/instances/{instance_id}/migrations", "world"},
	"getAdminInstance":                  {"GET", "/world/v1/admin/instances/{instance_id}", "world"},
	"getAdminMemoryBody":                {"GET", "/world/v1/admin/instances/{instance_id}/memories/{memory_id}/body", "memory"},
	"getAdminMemoryGovernance":          {"GET", "/world/v1/admin/instances/{instance_id}/memory-governance/{task_id}", "memory"},
	"getAdminSelfConsent":               {"GET", "/world/v1/admin/settings/self-consent", "consent"},
	"getAdminSettings":                  {"GET", "/world/v1/admin/settings", "settings"},
	"getAdminSettingsCommand":           {"GET", "/world/v1/admin/settings/commands/{command_key}", "settings"},
	"getAdminTask":                      {"GET", "/world/v1/admin/tasks/{task_id}", "task"},
	"getContentAsset":                   {"GET", "/world/v1/admin/assets/{asset_id}", "files"},
	"getContentDraft":                   {"GET", "/world/v1/admin/content-drafts/{draft_id}", "content"},
	"getContentRelease":                 {"GET", "/world/v1/admin/releases/{release_id}", "content"},
	"getCreatorCapabilities":            {"GET", "/world/v1/admin/capabilities", "content"},
	"getCustomerNotifications":          {"GET", "/world/v1/customer-notifications", "delivery"},
	"getEvents":                         {"GET", "/world/v1/instances/{instance_id}/events", "delivery"},
	"getInstance":                       {"GET", "/world/v1/instances/{instance_id}", "unavailable"},
	"getIntegrationCapabilities":        {"GET", "/world/v1/integration-capabilities", "store"},
	"getMemory":                         {"GET", "/world/v1/instances/{instance_id}/memories/{memory_id}", "memory"},
	"getOriginalTurnIntakeReceipt":      {"GET", "/world/v1/instances/{instance_id}/turn-intake-receipt", "store"},
	"getPlayerSelfConsent":              {"GET", "/world/v1/self-consent", "consent"},
	"getSnapshot":                       {"GET", "/world/v1/instances/{instance_id}/snapshot", "delivery"},
	"getTask":                           {"GET", "/world/v1/tasks/{task_id}", "task"},
	"getTurn":                           {"GET", "/world/v1/turns/{turn_id}", "delivery"},
	"getUsage":                          {"GET", "/world/v1/usage", "unavailable"},
	"governAdminMemory":                 {"POST", "/world/v1/admin/instances/{instance_id}/memory-governance", "memory"},
	"governMemories":                    {"POST", "/world/v1/instances/{instance_id}/memory-governance", "memory"},
	"listAdminActorInstances":           {"GET", "/world/v1/admin/instances/{instance_id}/actors", "world"},
	"listAdminInstances":                {"GET", "/world/v1/admin/instances", "world"},
	"listAdminMembers":                  {"GET", "/world/v1/admin/settings/members", "settings"},
	"listAdminMemoryMetadata":           {"GET", "/world/v1/admin/instances/{instance_id}/memories", "memory"},
	"listAdminTasks":                    {"GET", "/world/v1/admin/tasks", "task"},
	"listContentDrafts":                 {"GET", "/world/v1/admin/content-drafts", "content"},
	"listContentReleases":               {"GET", "/world/v1/admin/releases", "content"},
	"listDraftRevisions":                {"GET", "/world/v1/admin/content-drafts/{draft_id}/revisions", "content"},
	"listMemories":                      {"GET", "/world/v1/instances/{instance_id}/memories", "memory"},
	"previewInstanceMigration":          {"POST", "/world/v1/admin/instances/{instance_id}/migration-previews", "world"},
	"publishContentDraft":               {"POST", "/world/v1/admin/content-drafts/{draft_id}/releases", "content"},
	"readAuthorizedAsset":               {"GET", "/world/v1/assets/{asset_id}/content", "files"},
	"readRuntimeMedia":                  {"GET", "/world/v1/runtime-media/{asset_id}/content", "delivery"},
	"recordCustomerNotificationReceipt": {"POST", "/world/v1/customer-notifications/{notification_ref}/receipts", "delivery"},
	"rehearseContentDraft":              {"POST", "/world/v1/admin/content-drafts/{draft_id}/rehearsals", "rehearsal"},
	"resolveDraftGeneration":            {"POST", "/world/v1/admin/content-drafts/{draft_id}/generation-resolution", "candidate"},
	"saveContentDraft":                  {"PUT", "/world/v1/admin/content-drafts/{draft_id}", "content"},
	"setAdminMemoryUsePolicy":           {"POST", "/world/v1/admin/instances/{instance_id}/memory-use-policy", "memory"},
	"streamEvents":                      {"GET", "/world/v1/instances/{instance_id}/events/stream", "delivery"},
	"submitTurn":                        {"POST", "/world/v1/instances/{instance_id}/turns", "store"},
	"uploadContentAsset":                {"POST", "/world/v1/admin/assets", "files"},
}

var capabilityParameter = regexp.MustCompile(`\{[a-z_]+\}`)

func capabilityPattern(pattern string) string {
	return capabilityParameter.ReplaceAllString(pattern, "{}")
}

func (r *Runtime) capabilityOwners(ctx context.Context) map[string]bool {
	live := r.Store != nil && r.Store == r.creationStore && r.Local != nil && r.Local.Participant != nil && r.Store.Guard == r.Local.Participant && r.Local.Participant.Check(ctx) == nil
	owners := map[string]bool{}
	if !live {
		return owners
	}
	owners["store"] = true
	owners["files"] = r.Files != nil && r.Files.DB == r.Store
	owners["content"] = r.Content != nil && r.Content.Store == r.Store
	owners["memory"] = r.Memory != nil && r.Memory.Store == r.Store && r.Memory.Files == r.Files && r.Memory.Ledger == r.Local.Ledger && r.Memory.BindIntegrationGovernance() == nil
	owners["consent"] = owners["memory"] && r.Store.StudioConsentOwner == r.Memory
	owners["world"] = r.World != nil && r.World.Store == r.Store && r.World.InitialMemory == r.Memory && owners["memory"]
	owners["task"] = r.Worker != nil && r.Worker.s == r.Store && r.Worker.files == r.Files && r.Worker.Memory == r.Memory && owners["memory"]
	owners["delivery"] = r.Delivery != nil && r.World != nil && r.Turns != nil && r.Turns.Store == r.Store && owners["files"] && r.Store.ApprovedTurnReader == r.Files && r.Store.OriginalResultReader == r.Files && r.Store.CheckDeliveryReader(ctx, r.Delivery, r.World) == nil
	owners["settings"] = r.Auth != nil && r.Store.ConfigGuard == r.Local.Participant
	owners["candidate"] = r.Candidates != nil && r.Worker != nil && r.Worker.Turns != nil && r.Candidates.Executor == r.Worker.Turns && r.Worker.Turns.CandidateReady()
	owners["rehearsal"] = r.Worker != nil && r.Worker.Turns != nil && r.Worker.Turns.RehearsalReady()
	return owners
}

func (r *Runtime) integrationCapabilitiesRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /world/v1/integration-capabilities", func(w http.ResponseWriter, q *http.Request) {
		ctx, ok := r.integrationRuntimeCaller(w, q, false)
		if !ok {
			return
		}
		if len(q.URL.Query()) != 0 || generated.SourceHash != capabilityTableSourceHash {
			failure(w, wire.ErrJSON)
			return
		}
		profile, e := integrationProfile(q)
		if e != nil {
			failure(w, e)
			return
		}
		out := generated.IntegrationCapabilities{ContractVersion: "1.6.0", SourceHash: generated.SourceHash, SelectedProfile: profile, Profiles: json.RawMessage(capabilityProfiles), RuntimeMediaMimeTypes: []string{}, CommercialStatus: "SYNTHETIC_MOCK"}
		owners := r.capabilityOwners(ctx)
		values := reflect.ValueOf(&out.Operations).Elem()
		shape := values.Type()
		if values.NumField() != 67 || len(capabilityRoutes) != 67 {
			failure(w, wire.ErrJSON)
			return
		}
		for i := 0; i < values.NumField(); i++ {
			id := shape.Field(i).Tag.Get("json")
			route, present := capabilityRoutes[id]
			if !present {
				failure(w, wire.ErrJSON)
				return
			}
			state := "UNAVAILABLE"
			sample := capabilityParameter.ReplaceAllString(route.path, "capability-sample")
			probe, e := http.NewRequest(route.method, sample, nil)
			if e != nil {
				failure(w, e)
				return
			}
			_, pattern := mux.Handler(probe)
			// Parameter spelling may differ only in the established TurnId/task_id
			// locator. Method and every literal path segment still match exactly;
			// catch-all, subtree and missing routes never become AVAILABLE.
			matched := capabilityPattern(pattern) == capabilityPattern(route.method+" "+route.path)
			if matched && owners[route.group] {
				state = "AVAILABLE"
			}
			if route.group == "payment" {
				state = "PAYMENT_GATE_PENDING"
			}
			values.Field(i).SetString(state)
		}
		if out.Operations.ReadRuntimeMedia == "AVAILABLE" {
			out.RuntimeMediaMimeTypes = []string{"audio/wav"}
		}
		raw, e := json.Marshal(out)
		if e != nil || generated.Validate("IntegrationCapabilities", raw) != nil {
			failure(w, wire.ErrJSON)
			return
		}
		response(w, 200, out)
	})
}
