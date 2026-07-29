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
	"ANTHROPIC_API_KEY":           "anthropic_key_123",
	"OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318",
}

// optionalVars são as variáveis opcionais que o slice 12 acrescentou. Os testes
// as removem do ambiente para provar defaults e ausência de obrigatoriedade.
var optionalVars = []string{
	"PORT",
	"CLERK_ISSUER",
	"CLERK_WEBHOOK_SECRET",
	"S3_ENDPOINT",
	"S3_REGION",
	"S3_BUCKET",
	"S3_ACCESS_KEY",
	"S3_SECRET_KEY",
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

func TestLoad_OptionalFields_Defaults(t *testing.T) {
	// Só as `required` estão setadas; nenhuma variável opcional é exportada.
	setRequired(t)
	for _, k := range optionalVars {
		unset(t, k)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	// Port cai no default; os demais opcionais ficam vazios.
	if cfg.Port != "8080" {
		t.Errorf("Port = %q, want default %q", cfg.Port, "8080")
	}
	if cfg.ClerkIssuer != "" {
		t.Errorf("ClerkIssuer = %q, want empty", cfg.ClerkIssuer)
	}
	if cfg.ClerkWebhookSecret != "" {
		t.Errorf("ClerkWebhookSecret = %q, want empty", cfg.ClerkWebhookSecret)
	}
	if cfg.S3Bucket != "" {
		t.Errorf("S3Bucket = %q, want empty", cfg.S3Bucket)
	}
	if cfg.S3Enabled() {
		t.Error("S3Enabled() = true with no S3 vars set, want false")
	}
}

// TestLoad_OptionalFields_NotRequired garante que adicionar campos opcionais não
// tornou nenhum deles obrigatório: com todas as `required` setadas e todas as
// opcionais ausentes, Load ainda sobe. É a contra-prova da regra "existing
// lib/config tests stay green".
func TestLoad_OptionalFields_NotRequired(t *testing.T) {
	setRequired(t)
	for _, k := range optionalVars {
		unset(t, k)
	}

	if _, err := config.Load(); err != nil {
		t.Fatalf("Load() with optionals unset error = %v, want nil", err)
	}
}

// TestLoad_RequiredVarsUnchanged fixa o conjunto de variáveis obrigatórias: a
// extensão do slice 12 só acrescenta campos opcionais, então nenhuma nova
// variável pode ter entrado em requiredVars.
func TestLoad_RequiredVarsUnchanged(t *testing.T) {
	want := []string{
		"DATABASE_URL",
		"REDIS_URL",
		"CLERK_SECRET_KEY",
		"ANTHROPIC_API_KEY",
		"OTEL_EXPORTER_OTLP_ENDPOINT",
	}
	if len(requiredVars) != len(want) {
		t.Fatalf("requiredVars has %d entries, want %d", len(requiredVars), len(want))
	}
	for _, k := range want {
		if _, ok := requiredVars[k]; !ok {
			t.Errorf("requiredVars missing %q", k)
		}
	}
	// Nenhum campo opcional pode aparecer como obrigatório.
	for _, k := range optionalVars {
		if _, ok := requiredVars[k]; ok {
			t.Errorf("optional var %q leaked into requiredVars", k)
		}
	}
}

func TestLoad_S3Enabled_RequiresAllFields(t *testing.T) {
	full := map[string]string{
		"S3_REGION":     "us-east-1",
		"S3_BUCKET":     "jus-docs",
		"S3_ACCESS_KEY": "AKIA",
		"S3_SECRET_KEY": "secret",
	}

	tests := []struct {
		name string
		omit string // "" = nada omitido (config completa)
		want bool
	}{
		{name: "all fields set", omit: "", want: true},
		{name: "missing region", omit: "S3_REGION", want: false},
		{name: "missing bucket", omit: "S3_BUCKET", want: false},
		{name: "missing access key", omit: "S3_ACCESS_KEY", want: false},
		{name: "missing secret key", omit: "S3_SECRET_KEY", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequired(t)
			for k := range full {
				unset(t, k)
			}
			for k, v := range full {
				if k == tt.omit {
					continue
				}
				t.Setenv(k, v)
			}

			cfg, err := config.Load()
			if err != nil {
				t.Fatalf("Load() error = %v, want nil", err)
			}
			if got := cfg.S3Enabled(); got != tt.want {
				t.Errorf("S3Enabled() = %v, want %v", got, tt.want)
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
