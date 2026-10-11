package config

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// Secret prevents accidental logging. Reveal is reserved for database construction.
type Secret struct{ value string }

func (s Secret) Reveal() string                { return s.value }
func (s Secret) String() string                { return "[REDACTED]" }
func (s Secret) GoString() string              { return s.String() }
func (s Secret) Format(f fmt.State, verb rune) { _, _ = io.WriteString(f, s.String()) }

type Database struct {
	Addr, Name, User, PasswordRef string
	Password                      Secret
}
type Config struct {
	GovernanceRoot                                                                            string
	Mode, ListenAddr, FilesRoot, ModelMode, BillingMode, PublicBaseURL                        string
	Database                                                                                  Database
	OperatorTenant, OperatorProduct, OperatorSubject, OperatorCredentialID, OperatorSecretRef string
	OperatorSecret                                                                            Secret
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func LoadConfig(lookup func(string) (string, bool)) (Config, error) {
	if lookup == nil {
		return Config{}, errors.New("configuration lookup required")
	}
	get := func(key, def string) string {
		v, ok := lookup(key)
		if !ok {
			return def
		}
		return v
	}
	cfg := Config{Mode: get("LOREVIVA_MODE", "all"), ListenAddr: get("LOREVIVA_HTTP_ADDR", "127.0.0.1:8080"), FilesRoot: get("LOREVIVA_FILES_ROOT", "./var/private-files"), ModelMode: get("LOREVIVA_MODEL_MODE", "mock"), BillingMode: get("LOREVIVA_BILLING_MODE", "mock"), PublicBaseURL: get("LOREVIVA_PUBLIC_BASE_URL", "http://127.0.0.1:8080")}
	cfg.GovernanceRoot = get("LOREVIVA_GOVERNANCE_ROOT", cfg.FilesRoot+".independent")
	if cfg.Mode != "all" && cfg.Mode != "api" && cfg.Mode != "worker" {
		return Config{}, errors.New("LOREVIVA_MODE must be all, api or worker")
	}
	if cfg.ModelMode != "mock" || cfg.BillingMode != "mock" {
		return Config{}, errors.New("R0 requires mock model and billing modes")
	}
	for key, dest := range map[string]*string{"LOREVIVA_DB_ADDR": &cfg.Database.Addr, "LOREVIVA_DB_NAME": &cfg.Database.Name, "LOREVIVA_DB_USER": &cfg.Database.User, "LOREVIVA_DB_PASSWORD_REF": &cfg.Database.PasswordRef} {
		*dest = get(key, "")
		if strings.TrimSpace(*dest) == "" {
			return Config{}, errors.New(key + " is required")
		}
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		return Config{}, errors.New("LOREVIVA_HTTP_ADDR must be host:port")
	}
	if _, _, err := net.SplitHostPort(cfg.Database.Addr); err != nil {
		return Config{}, errors.New("LOREVIVA_DB_ADDR must be host:port")
	}
	if strings.TrimSpace(cfg.FilesRoot) == "" {
		return Config{}, errors.New("LOREVIVA_FILES_ROOT is required")
	}
	u, err := url.Parse(cfg.PublicBaseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return Config{}, errors.New("LOREVIVA_PUBLIC_BASE_URL must be an HTTP(S) URL without credentials")
	}
	ref := cfg.Database.PasswordRef
	if !strings.HasPrefix(ref, "env:") || !envKey.MatchString(strings.TrimPrefix(ref, "env:")) {
		return Config{}, errors.New("database secret reference must name an explicit environment key")
	}
	value, ok := lookup(strings.TrimPrefix(ref, "env:"))
	if !ok || value == "" {
		return Config{}, errors.New("database secret reference did not resolve")
	}
	cfg.Database.Password = Secret{value: value}
	cfg.OperatorTenant = get("LOREVIVA_OPERATOR_TENANT", "demo-local")
	cfg.OperatorProduct = get("LOREVIVA_OPERATOR_PRODUCT", "coffee")
	cfg.OperatorSubject = get("LOREVIVA_OPERATOR_SUBJECT", "creator")
	cfg.OperatorCredentialID = get("LOREVIVA_OPERATOR_CREDENTIAL_ID", "owner")
	cfg.OperatorSecretRef = get("LOREVIVA_OPERATOR_SECRET_REF", "")
	if cfg.OperatorSecretRef != "" {
		ref := cfg.OperatorSecretRef
		if !strings.HasPrefix(ref, "env:") || !envKey.MatchString(strings.TrimPrefix(ref, "env:")) {
			return Config{}, errors.New("operator secret requires explicit environment reference")
		}
		secret, ok := lookup(strings.TrimPrefix(ref, "env:"))
		if !ok || len(secret) < 32 || len(secret) > 512 {
			return Config{}, errors.New("operator secret reference did not resolve to a bounded credential")
		}
		cfg.OperatorSecret = Secret{value: secret}
	}
	return cfg, nil
}
