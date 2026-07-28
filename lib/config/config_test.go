package config_test

import (
	"errors"
	"os"
	"testing"

	"github.com/caarlos0/env/v11"
	"github.com/jusassessoria/platform/lib/config"
)

// requiredVars mapeia cada variável `required` a um valor válido de teste.
var requiredVars = map[string]string{
	"DATABASE_URL":                "postgres://user:pass@localhost:5432/jus",
	"REDIS_URL":                   "redis://localhost:6379/0",
	"CLERK_SECRET_KEY":            "sk_test_123",
	"CLERK_JWKS_URL":              "https://clerk.example.com/.well-known/jwks.json",
	"ANTHROPIC_API_KEY":           "anthropic_key_123",
	"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
}

// setRequired popula todas as variáveis `required` com valores válidos.
func setRequired(t *testing.T) {
	t.Helper()
	for k, v := range requiredVars {
		t.Setenv(k, v)
	}
}

// unset remove uma variável e a restaura no fim do teste. t.Setenv não oferece
// "unset", e para o `required` presente-mas-vazio conta como setado — só a
// ausência real dispara VarIsNotSetError.
func unset(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%q) = %v", key, err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestLoad_AllRequiredSet_DefaultsEnv(t *testing.T) {
	setRequired(t)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.DatabaseURL != requiredVars["DATABASE_URL"] {
		t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, requiredVars["DATABASE_URL"])
	}
	if cfg.RedisURL != requiredVars["REDIS_URL"] {
		t.Errorf("RedisURL = %q, want %q", cfg.RedisURL, requiredVars["REDIS_URL"])
	}
	if cfg.ClerkSecret != requiredVars["CLERK_SECRET_KEY"] {
		t.Errorf("ClerkSecret = %q, want %q", cfg.ClerkSecret, requiredVars["CLERK_SECRET_KEY"])
	}
	if cfg.ClerkJWKSURL != requiredVars["CLERK_JWKS_URL"] {
		t.Errorf("ClerkJWKSURL = %q, want %q", cfg.ClerkJWKSURL, requiredVars["CLERK_JWKS_URL"])
	}
	if cfg.AnthropicKey != requiredVars["ANTHROPIC_API_KEY"] {
		t.Errorf("AnthropicKey = %q, want %q", cfg.AnthropicKey, requiredVars["ANTHROPIC_API_KEY"])
	}
	if cfg.OTELEndpoint != requiredVars["OTEL_EXPORTER_OTLP_ENDPOINT"] {
		t.Errorf("OTELEndpoint = %q, want %q", cfg.OTELEndpoint, requiredVars["OTEL_EXPORTER_OTLP_ENDPOINT"])
	}

	// APP_ENV não foi setado -> default "development".
	if cfg.Env != "development" {
		t.Errorf("Env = %q, want default %q", cfg.Env, "development")
	}
	if cfg.IsProduction() {
		t.Error("IsProduction() = true, want false for default env")
	}
}

func TestLoad_MissingRequired_ReturnsError(t *testing.T) {
	for missing := range requiredVars {
		t.Run(missing, func(t *testing.T) {
			setRequired(t)
			unset(t, missing)

			_, err := config.Load()
			if err == nil {
				t.Fatalf("Load() with %s unset: error = nil, want VarIsNotSetError", missing)
			}
			if !errors.Is(err, env.VarIsNotSetError{}) {
				t.Errorf("Load() error = %v, want errors.Is VarIsNotSetError", err)
			}
		})
	}
}

func TestLoad_AppEnvOverridesDefault(t *testing.T) {
	setRequired(t)
	t.Setenv("APP_ENV", "production")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.Env != "production" {
		t.Errorf("Env = %q, want %q", cfg.Env, "production")
	}
	if !cfg.IsProduction() {
		t.Error("IsProduction() = false, want true when APP_ENV=production")
	}
}
