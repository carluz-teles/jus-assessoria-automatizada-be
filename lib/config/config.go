// Package config carrega a configuração do processo a partir de variáveis de
// ambiente (12-factor). Todo binário chama Load() no passo 1 do boot; se uma
// variável `required` faltar, Load devolve erro e o main faz Fatal — melhor
// morrer na subida do que quebrar num request três horas depois.
//
// Fonte de verdade da struct: docs/erd-backend.md §5b.4.
package config

import (
	"log/slog"
	"os"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Config é a configuração tipada do processo. Segredos (ClerkSecret,
// AnthropicKey) chegam só pelo ambiente — nunca no código nem no repositório.
//
// Os campos `required` morrem no boot se faltarem (fail fast, docs §5b.4). Os
// demais são opcionais com default sensato: o binário que precisa deles valida
// no ponto de uso (ex.: storage.New exige bucket/região/credenciais). Manter os
// novos campos opcionais preserva o contrato dos binários que não os usam — um
// worker não deve morrer por não ter S3 configurado.
type Config struct {
	DatabaseURL  string `env:"DATABASE_URL,required"`
	RedisURL     string `env:"REDIS_URL,required"`
	ClerkSecret  string `env:"CLERK_SECRET_KEY,required"`
	AnthropicKey string `env:"ANTHROPIC_API_KEY,required"`
	OTELEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT,required"`
	Env          string `env:"APP_ENV" envDefault:"development"`

	// Exportação OTLP — opcionais, com default seguro para produção (New Relic):
	// sem headers e com TLS ligado. OTELHeaders é a string crua no formato da spec
	// (`key1=value1,key2=value2`, ex.: `api-key=<license>`); vazia = nenhum header.
	// OTELInsecure default false mantém o TLS ligado (spec-compliant, exigido pelo
	// New Relic); o dev local com collector sem TLS seta INSECURE=true.
	OTELHeaders  string `env:"OTEL_EXPORTER_OTLP_HEADERS"`
	OTELInsecure bool   `env:"OTEL_EXPORTER_OTLP_INSECURE" envDefault:"false"`

	// LogLevel — nível mínimo de log do processo (todos os binários). Default "info";
	// "debug" liga os logs por-evento do relay e o diagnóstico geral sem redeploy de
	// código. Valores: debug|info|warn|error (case-insensitive); um valor desconhecido
	// cai em info — um nível mal digitado nunca deve derrubar o boot. Ver SlogLevel().
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	// HTTP + Clerk auth (só o api os consome). Port tem default; o issuer e o
	// segredo do webhook são opcionais — issuer vazio deixa o ClerkVerifier
	// aceitar o issuer padrão da instância, segredo vazio só quebra ao verificar
	// um webhook, não no boot.
	Port               string `env:"PORT" envDefault:"8080"`
	ClerkIssuer        string `env:"CLERK_ISSUER"`
	ClerkWebhookSecret string `env:"CLERK_WEBHOOK_SECRET"`

	// CORS — origens de browser permitidas (comma-separated), só o api consome. O FE
	// (Next.js em outra origem) chama o api com header Authorization, o que torna todo
	// request "não-simples" e dispara preflight: sem Access-Control-Allow-Origin o
	// browser descarta a resposta. O default cobre o domínio de prod (app.atjud.com.br),
	// o subdomínio Railway (legado) e o dev local; produção pode sobrescrever via env.
	CORSAllowedOrigins string `env:"CORS_ALLOWED_ORIGINS" envDefault:"https://app.atjud.com.br,https://autojus-web.up.railway.app,http://localhost:3000"`

	// Stripe billing (só o api os consome, no webhook /webhooks/stripe). Opcionais
	// pelo mesmo motivo do ClerkWebhookSecret: um segredo vazio só quebra ao
	// verificar/resolver um webhook, não no boot — e mantê-los opcionais preserva o
	// boot dos demais binários (worker/scheduler), que não falam com o Stripe.
	StripeSecretKey     string `env:"STRIPE_SECRET_KEY"`
	StripeWebhookSecret string `env:"STRIPE_WEBHOOK_SECRET"`

	// Billing checkout/portal redirects + trial (só o api os consome, nos endpoints
	// /v1/billing/checkout|portal). Opcionais: um destino vazio só produz um Checkout
	// sem redirect útil, não quebra o boot dos demais binários. STRIPE_TRIAL_DAYS
	// default 0 = sem trial.
	BillingSuccessURL string `env:"APP_BILLING_SUCCESS_URL"`
	BillingCancelURL  string `env:"APP_BILLING_CANCEL_URL"`
	BillingReturnURL  string `env:"APP_BILLING_RETURN_URL"`
	StripeTrialDays   int    `env:"STRIPE_TRIAL_DAYS" envDefault:"0"`

	// Resend — provedor de e-mail transacional (só o worker que roda o listener de
	// notifications os consome). Opcionais no agregado pelo mesmo motivo do Stripe/S3:
	// mantê-los opcionais preserva o boot dos demais binários (api/scheduler/relay),
	// que não enviam e-mail. O worker de notifications valida a presença no boot
	// (NewResendClient/NewEmailChannel devolvem Invalid) e falha rápido se faltarem.
	ResendAPIKey    string `env:"RESEND_API_KEY"`
	ResendFromEmail string `env:"RESEND_FROM_EMAIL"`
	// ResendWebhookSecret assina os webhooks de bounce/complaint (svix). Opcional
	// pelo mesmo motivo do ClerkWebhookSecret: um segredo vazio só quebra ao
	// verificar um webhook, não no boot — e mantê-lo opcional preserva o boot dos
	// demais binários, que não recebem webhook do Resend. Só o api o consome.
	ResendWebhookSecret string `env:"RESEND_WEBHOOK_SECRET"`

	// Object storage S3-compatível (S3/R2/MinIO). Opcional: o api só monta o
	// storage.Client quando S3Enabled() — ver o método abaixo.
	S3Endpoint  string `env:"S3_ENDPOINT"`
	S3Region    string `env:"S3_REGION"`
	S3Bucket    string `env:"S3_BUCKET"`
	S3AccessKey string `env:"S3_ACCESS_KEY"`
	S3SecretKey string `env:"S3_SECRET_KEY"`

	// Calendário de prazos.
	// HolidaySeedYearsAhead — quantos anos ALÉM do corrente o seeder nacional de
	// feriados pré-carrega no boot do api (via BrasilAPI). 2 = ano corrente + 2.
	HolidaySeedYearsAhead int `env:"HOLIDAY_SEED_YEARS_AHEAD" envDefault:"2"`

	// Conector DJEN — proxy de saída. O WAF do Comunica (DJEN) 403 o IP de
	// datacenter do Railway; um proxy residencial/BR dá um IP de saída limpo.
	// Opcional: vazio = conexão direta (o dev local, cujo IP já passa no WAF, não
	// precisa). Só o worker-ingestao — onde o DJENConnector roda o backfill — o consome.
	DJENProxyURL string `env:"DJEN_PROXY_URL"`
}

// Load lê o ambiente para uma Config. Devolve o erro (não faz panic): o boot do
// binário decide o Fatal, e os testes precisam poder inspecionar a falha. Uma
// variável `required` ausente vem como env.VarIsNotSetError dentro do agregado.
func Load() (Config, error) {
	return env.ParseAs[Config]()
}

// IsProduction indica se o processo roda em produção (APP_ENV=production).
func (c Config) IsProduction() bool {
	return c.Env == "production"
}

// LogLevelFromEnv lê SÓ a LOG_LEVEL do ambiente (sem o Load completo) para o
// boot-logger de cada binário nascer já no nível certo — o logger é criado em
// main(), antes de Load validar o resto do ambiente. Reusa a mesma tabela de
// SlogLevel (uma fonte de verdade), então default/desconhecido = info.
func LogLevelFromEnv() slog.Level {
	return Config{LogLevel: os.Getenv("LOG_LEVEL")}.SlogLevel()
}

// SlogLevel converte LogLevel na slog.Level correspondente. Um valor desconhecido
// (ou vazio) cai em info: o boot nunca deve morrer por um nível mal digitado.
func (c Config) SlogLevel() slog.Level {
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// S3Enabled informa se o storage de objetos está totalmente configurado. O api
// só monta o storage.Client quando todos os campos exigidos por storage.New
// estão presentes (endpoint é opcional: vazio = AWS real). Configuração parcial
// conta como desabilitada em vez de falhar o boot — a feature que usa S3 chega
// depois; até lá o binário sobe sem ele.
func (c Config) S3Enabled() bool {
	return c.S3Bucket != "" &&
		c.S3Region != "" &&
		c.S3AccessKey != "" &&
		c.S3SecretKey != ""
}
