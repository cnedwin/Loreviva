package domain

import "errors"

type SettingsScope struct {
	Kind      string  `json:"scope_type"`
	TenantID  string  `json:"tenant_id"`
	ProductID *string `json:"product_id,omitempty"`
}

// Validate the scope branch before using its address: TENANT excludes a product, while PRODUCT requires a valid product ID. The address itself grants no authority.
func (s SettingsScope) Validate() error {
	if ValidateID(s.TenantID) != nil {
		return errors.New("invalid settings tenant")
	}
	if s.Kind == "TENANT" && s.ProductID == nil {
		return nil
	}
	if s.Kind == "PRODUCT" && s.ProductID != nil && ValidateID(*s.ProductID) == nil {
		return nil
	}
	return errors.New("invalid settings scope branch")
}
func (s SettingsScope) Address() string {
	if s.Kind == "TENANT" {
		return "T"
	}
	if s.ProductID != nil {
		return "P:" + *s.ProductID
	}
	return ""
}
func (s SettingsScope) ProductValue() any {
	if s.ProductID == nil {
		return nil
	}
	return *s.ProductID
}
func (s SettingsScope) SettingsUse() string {
	if s.Kind == "TENANT" {
		return "TENANT_SETTINGS"
	}
	return "PRODUCT_SETTINGS"
}
func StudioSettingsAction(action string) bool {
	switch action {
	case "settings.read", "settings.defaults.write", "settings.quota.write", "settings.connection.write", "settings.notifications.write", "members.manage":
		return true
	}
	return false
}
