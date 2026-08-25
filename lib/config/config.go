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

	// OTELTracesMode controla o sampling de traces (deny-by-default). "import-only"
	// (default) mantém no backend SÓ o fluxo de import — corta o flood de preflight,
	// polling e reads que afogava o painel (e o custo de ingest no New Relic); "all"
	// volta a amostrar tudo, a alavanca de incidente pra investigar um fluxo ainda não
	// instrumentado sem redeploy. Valor desconhecido cai em import-only (o padrão
	// seguro). Ver TracesImportOnly() e lib/telemetry.buildSampler.
	OTELTracesMode string `env:"OTEL_TRACES_MODE" envDefault:"import-only"`

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

	// Voyage — provedor de embeddings da fatia de indexação/retrieval de documentos
	// (só o worker-documents, onde roda o listener de indexing, os consome). O
	// milestone escolheu Voyage voyage-3.5-lite/vector(1024) (Anthropic não tem API de
	// embeddings). Opcionais no agregado pelo mesmo motivo do Stripe/S3/Resend:
	// mantê-los opcionais preserva o boot dos demais binários (api/scheduler/relay),
	// que não geram embeddings. O adapter Voyage valida a presença da chave no ponto
	// de uso (Embed devolve Invalid) e falha rápido se faltar. VOYAGE_MODEL/EMBED_DIM
	// têm default sensato (o modelo/dim do milestone), então só a chave é um segredo.
	VoyageAPIKey   string `env:"VOYAGE_API_KEY"`
	VoyageModel    string `env:"VOYAGE_MODEL" envDefault:"voyage-4-lite"`
	VoyageEmbedDim int    `env:"VOYAGE_EMBED_DIM" envDefault:"1024"`

	// OCREngine seleciona o adapter de OCR da fatia de extração (só o worker-documents o
	// consome). "tesseract" (default) roda OCR determinístico e GRÁTIS por shell-out
	// (pdftoppm + tesseract, embutidos na imagem runtime-ocr) — sem chave, sem custo por
	// página; é o que lê os scans do PJe. "claude" volta pro OCR via Claude vision, que
	// exige ANTHROPIC_API_KEY e cobra por documento (opt-in, pra casos que o Tesseract não
	// resolve). Um valor desconhecido cai no Tesseract (o padrão seguro/grátis), então um
	// típo nunca liga o custo por engano.
	OCREngine string `env:"OCR_ENGINE" envDefault:"tesseract"`

	// OpenRouter — provedor de GERAÇÃO (LLM) do advisory. Roteia N modelos num endpoint
	// OpenAI-compatível; começamos com um modelo LIGHT baratíssimo pra tarefas estreitas +
	// structured output (ex.: intimação → tarefas sugeridas). Só a chave é segredo; MODEL e
	// BASE_URL têm default sensato, trocáveis por env sem redeploy (o tiering do §7: light aqui,
	// strong pra peça). Opcional no agregado — o adapter valida a chave no ponto de uso (Generate
	// devolve Invalid) e o binário que não gera não morre por falta dela.
	OpenRouterAPIKey  string `env:"OPENROUTER_API_KEY"`
	OpenRouterModel   string `env:"OPENROUTER_MODEL" envDefault:"openai/gpt-4o-mini"`
	OpenRouterBaseURL string `env:"OPENROUTER_BASE_URL" envDefault:"https://openrouter.ai/api/v1"`

	// Calendário de prazos.
	// HolidaySeedYearsAhead — quantos anos ALÉM do corrente o seeder nacional de
	// feriados pré-carrega no boot do api (via BrasilAPI). 2 = ano corrente + 2.
	HolidaySeedYearsAhead int `env:"HOLIDAY_SEED_YEARS_AHEAD" envDefault:"2"`

	// Conector DJEN — proxy de saída. O WAF do Comunica (DJEN) 403 o IP de
	// datacenter do Railway; um proxy residencial/BR dá um IP de saída limpo.
	// Opcional: vazio = conexão direta (o dev local, cujo IP já passa no WAF, não
	// precisa). Só o worker-ingestao — onde o DJENConnector roda o backfill — o consome.
	DJENProxyURL string `env:"DJEN_PROXY_URL"`

	// DJENRatePerMinute — teto de requisições/min do conector DJEN (shared limiter).
	// 0 = mantém o default do conector (60/min). Alavanca de prod pra afrouxar o pace
	// num sweep de OAB grande sem redeploy, quando o DJEN passa a 429 a captura.
	DJENRatePerMinute int `env:"DJEN_RATE_PER_MINUTE" envDefault:"0"`

	// DJENPageSize — itens por página na consulta DJEN. 0 = mantém o default do
	// conector (1000, o teto que a Comunica honra). Página maior = menos requests por
	// janela, então menos pressão no pace/429. Alavanca de tuning sem redeploy.
	DJENPageSize int `env:"DJEN_PAGE_SIZE" envDefault:"0"`

	// BackfillWindowDays — quantos dias cada fatia do backfill cobre. Default 7
	// (semanal, 53 fatias no ano): o benchmark mostrou que a latência do DJEN escala
	// com o intervalo da janela, então alargar não reduz o total — só reempacota o
	// mesmo scan em requests maiores. Com o fetch agora PARALELO, mais fatias menores
	// paralelizam melhor e dão recent-first mais granular (as últimas semanas caem no
	// 1º lote). Alavanca de tuning sem redeploy. 0/negativo = default do use case.
	BackfillWindowDays int `env:"BACKFILL_WINDOW_DAYS" envDefault:"7"`

	// IngestaoConcurrency — goroutines simultâneas do worker-ingestao. Default 8: os
	// fetches DJEN/DATAJUD são LONGOS (~110s cada, presos no proxy residencial), então
	// rodá-los em paralelo sobrepõe essa espera e derruba o wall-clock do backfill ~N×;
	// o limiter de 1/s só espaça o INÍCIO dos requests, não impede vários em voo. As
	// escritas concorrentes que isso destravaria são serializadas pelo advisory-lock
	// por tenant (sem deadlock 40P01). Alavanca de tuning sem redeploy.
	IngestaoConcurrency int `env:"INGESTAO_CONCURRENCY" envDefault:"8"`

	// IngestionEnabled liga a ingestão nacional por-tribunal no scheduler (a virada
	// bulk). Default FALSE: o código pode ser deployado inerte, coexistindo com o
	// per-OAB, e a gente vira a chave só quando quiser cortar. Só o scheduler lê.
	IngestionEnabled bool `env:"INGESTION_ENABLED" envDefault:"false"`

	// IngestionLookbackDays — quantos dias pra trás de "ontem" a ingestão diária
	// re-varre a cada dia, pra pegar publicações atrasadas/retratadas (data_cancelamento).
	// Idempotente (ON CONFLICT no hash), então re-varrer é seguro. Default 3.
	IngestionLookbackDays int `env:"INGESTION_LOOKBACK_DAYS" envDefault:"3"`

	// BootstrapDays — quando > 0, o scheduler ingere os últimos N dias do diário
	// nacional UMA vez no boot, em background (o cold-start do store). Ops seta (ex.
	// 90) pro primeiro carregamento e volta pra 0 depois. Default 0 = sem bootstrap.
	BootstrapDays int `env:"BOOTSTRAP_DAYS" envDefault:"0"`

	// BillingGateEnabled liga o enforcement do limite de processos ativos
	// (active_process_limit, billing.EntitlementAdapter) na ativação de integração
	// (api) e no ciclo de sync (worker-ingestao). TEMPORÁRIO: a precificação dos
	// planos ainda não foi decidida (decisão de negócio pendente), então o default é
	// FALSE — ninguém é bloqueado até lá. A lógica de enforcement em si (entity.go,
	// domain.go, entitlement.go) continua intacta e testada; esta flag só decide se a
	// composição em cmd/api e cmd/worker-ingestao a invoca ou injeta um checker
	// ilimitado no lugar (acquisition.NewUnlimitedEntitlementChecker). Religar (ou
	// remover a flag, se ela deixar de fazer sentido) quando a precificação for
	// definida.
	BillingGateEnabled bool `env:"BILLING_GATE_ENABLED" envDefault:"false"`

	// CaptureDailyTime — horário (HH:MM) da captura diária, exposto na tela "Capturas"
	// como "próxima execução" (o FE formata "amanhã, HH:MM"). Opcional e HONESTO: só o
	// api o lê para o read model; vazio = a tela mostra "—" (sem inventar horário a
	// partir de time.Now()). Espelha o cron da captura diária do scheduler quando ops
	// quiser mostrá-lo; um valor mal-formado é tratado como vazio no ponto de uso.
	CaptureDailyTime string `env:"CAPTURE_DAILY_TIME"`

	// Certificado digital A1 (slice certificate) — cofre de chave (envelope
	// encryption). NÃO são globalmente `required`: o slice valida na sua própria
	// construção (lazy), então os demais binários sobem sem eles. Seleção do cofre:
	// AWS_KMS_KEY_ID setado → cofre KMS (prod); senão CERT_KEK → cofre local (dev);
	// nenhum dos dois → o slice fica desmontado (sem endpoint de certificado).
	//
	// CERT_KEK é uma KEK AES-256 em base64 (32 bytes decodificados) — segredo, só
	// por env. AWS_KMS_KEY_ID é o id/ARN da CMK; a região do KMS reusa S3Region
	// quando CERT_KMS_REGION é vazio, e as credenciais reusam S3AccessKey/S3SecretKey
	// (mesmo provider AWS/R2 já configurado) quando não há credencial dedicada.
	CertKEK       string `env:"CERT_KEK"`
	AWSKMSKeyID   string `env:"AWS_KMS_KEY_ID"`
	CertKMSRegion string `env:"CERT_KMS_REGION"`

	// VaultKEK is the base64-encoded 32-byte Key Encryption Key lib/vault uses to
	// wrap every secret's per-row DEK (docs/erd-execucao-judicial-tjsp.md §9 —
	// court_connection.credential_ref is the first secret it protects). Optional in
	// the aggregate for the same reason S3/Resend/Stripe are: only the api (where
	// the court credential endpoints live) needs it, and keeping it optional here
	// preserves the boot of every binary that doesn't touch the vault. The slice's
	// own wiring in cmd/api validates presence at the point of use (vault.New
	// returns vault.ErrInvalidKEK) and fails fast there, not mid-request — same
	// convention as storage.New/S3Enabled.
	VaultKEK string `env:"VAULT_KEK_BASE64"`
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

// TracesImportOnly informa se o processo amostra SÓ o fluxo de import (deny-by-default).
// Só "all" (case-insensitive) desliga o filtro; qualquer outro valor — inclusive um
// típo — mantém o padrão seguro import-only. Ver lib/telemetry.buildSampler.
func (c Config) TracesImportOnly() bool {
	return strings.ToLower(strings.TrimSpace(c.OTELTracesMode)) != "all"
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

// Modo do cofre do certificado A1 (slice certificate).
const (
	// CertVaultKMS — cofre KMS (produção): AWS_KMS_KEY_ID setado.
	CertVaultKMS = "kms"
	// CertVaultLocal — cofre local (dev): sem KMS, mas CERT_KEK presente.
	CertVaultLocal = "local"
	// CertVaultDisabled — nenhum cofre configurado: o slice fica desmontado.
	CertVaultDisabled = "disabled"
)

// CertificateVaultMode seleciona o cofre do slice de certificado a partir do
// ambiente, sem tocar em nada globalmente `required`: KMS vence quando
// AWS_KMS_KEY_ID está setado (prod); senão CERT_KEK habilita o cofre local (dev);
// se nenhum dos dois estiver presente o slice não é montado (o api sobe sem o
// endpoint de certificado, como faz com S3). A validação profunda (KEK base64 de
// 32 bytes, região do KMS) acontece na construção do cofre, não aqui.
func (c Config) CertificateVaultMode() string {
	switch {
	case c.AWSKMSKeyID != "":
		return CertVaultKMS
	case c.CertKEK != "":
		return CertVaultLocal
	default:
		return CertVaultDisabled
	}
}

// CertificateKMSRegion resolve a região do KMS: CERT_KMS_REGION quando setada,
// senão S3Region (o mesmo provider AWS/R2 já configurado). Vazia deixa a
// construção do cofre KMS falhar com um erro tipado — melhor do que adivinhar.
func (c Config) CertificateKMSRegion() string {
	if c.CertKMSRegion != "" {
		return c.CertKMSRegion
	}
	return c.S3Region
}
