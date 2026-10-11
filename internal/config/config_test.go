package config

import (
	"fmt"
	"strings"
	"testing"
)

func validEnv() map[string]string {
	return map[string]string{
		"LOREVIVA_DB_ADDR": "127.0.0.1:3306", "LOREVIVA_DB_NAME": "loreviva_test", "LOREVIVA_DB_USER": "test_user",
		"LOREVIVA_DB_PASSWORD_REF": "env:TEST_DB_PASSWORD", "TEST_DB_PASSWORD": "synthetic-never-log-me",
	}
}
func lookup(env map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := env[k]; return v, ok }
}
func TestLoadConfigRejectsMissingSecretRefAndBadMode(t *testing.T) {
	for _, test := range []struct{ key, value string }{{"LOREVIVA_DB_PASSWORD_REF", ""}, {"LOREVIVA_MODE", "bogus"}, {"LOREVIVA_DB_PASSWORD_REF", "env:MISSING"}, {"LOREVIVA_DB_PASSWORD_REF", "literal:synthetic-never-log-me"}, {"LOREVIVA_MODEL_MODE", "live"}, {"LOREVIVA_BILLING_MODE", "paid"}} {
		env := validEnv()
		env[test.key] = test.value
		_, err := LoadConfig(lookup(env))
		if err == nil {
			t.Fatalf("accepted invalid %s", test.key)
		}
		if strings.Contains(err.Error(), env["TEST_DB_PASSWORD"]) {
			t.Fatal("secret in error")
		}
	}
}
func TestLoadConfigDefaultsAndRedaction(t *testing.T) {
	cfg, err := LoadConfig(lookup(validEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Mode != "all" || cfg.ListenAddr != "127.0.0.1:8080" {
		t.Fatal("unsafe defaults")
	}
	if cfg.Database.Password.Reveal() != "synthetic-never-log-me" {
		t.Fatal("explicit reference not resolved")
	}
	for _, s := range []string{fmt.Sprint(cfg), fmt.Sprintf("%+v", cfg), fmt.Sprintf("%#v", cfg), fmt.Sprintf("%#v", cfg.Database.Password)} {
		if strings.Contains(s, "synthetic-never-log-me") {
			t.Fatal("secret formatted")
		}
	}
}
func TestLoadConfigRejectsMissingDatabaseAndBadAddress(t *testing.T) {
	for _, key := range []string{"LOREVIVA_DB_ADDR", "LOREVIVA_DB_NAME", "LOREVIVA_DB_USER"} {
		env := validEnv()
		delete(env, key)
		if _, err := LoadConfig(lookup(env)); err == nil {
			t.Fatalf("missing %s", key)
		}
	}
	env := validEnv()
	env["LOREVIVA_HTTP_ADDR"] = "invalid"
	if _, err := LoadConfig(lookup(env)); err == nil {
		t.Fatal("bad address")
	}
}
