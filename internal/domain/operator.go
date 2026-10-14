package domain

// OperatorPrincipal is a read-only description. Owning authority is the private
// Store context minted from an actual bearer lookup, never this caller value.
type OperatorPrincipal struct {
	CredentialID string
	ProductScope
}

const (
	StudioCredentialLock = "studio_credential"
	StudioGrantsLock     = "studio_grants"
	StudioScopeLock      = "studio_scope"
	StudioConnectionLock = "studio_connection"
	StudioSettingsLock   = "studio_settings"
	StudioConsentLock    = "studio_consent"
)

func ProductAdminAction(action string) bool {
	switch action {
	case "content.read", "content.edit", "content.copy", "content.archive", "content.restore", "content.delete", "content.check", "content.publish", "content.rehearse", "content.assist", "task.read", "task.cancel", "asset.upload", "asset.read", "runtime.create", "runtime.read", "runtime.control", "runtime.migrate":
		return true
	}
	return false
}
