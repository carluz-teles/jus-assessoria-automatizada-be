// Package config carrega a configuração do processo a partir de variáveis de
// ambiente (12-factor). Todo binário chama Load() no passo 1 do boot; se uma
// variável `required` faltar, Load devolve erro e o main faz Fatal — melhor
// morrer na subida do que quebrar num request três horas depois.
//
// Fonte de verdade da struct: docs/erd-backend.md §5b.4.
package config

import "github.com/caarlos0/env/v11"

// Config é a configuração tipada do processo. Segredos (ClerkSecret,
// AnthropicKey) chegam só pelo ambiente — nunca no código nem no repositório.
type Config struct {
	DatabaseURL  string `env:"DATABASE_URL,required"`
	RedisURL     string `env:"REDIS_URL,required"`
	ClerkSecret  string `env:"CLERK_SECRET_KEY,required"`
	ClerkJWKSURL string `env:"CLERK_JWKS_URL,required"`
	AnthropicKey string `env:"ANTHROPIC_API_KEY,required"`
	OTELEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT,required"`
	Env          string `env:"APP_ENV" envDefault:"development"`
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
