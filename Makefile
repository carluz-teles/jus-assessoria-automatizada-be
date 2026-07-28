.PHONY: build test test-integration tidy sqlc docker-build up down itest-db

# Green gate: compila tudo.
build:
	go build ./...

# Green gate: roda os testes rápidos (unitários). O tag `integration` fica de
# fora — ver test-integration.
test:
	go test ./...

# Testes de integração: repo/pipeline contra Postgres real, subido pelo próprio
# teste via testcontainers (build tag `integration`). Prova migrate + RLS.
test-integration:
	go test -tags=integration ./...

# Sincroniza go.mod/go.sum.
tidy:
	go mod tidy

# Gera código a partir do SQL (schema em migrations/, queries por slice).
sqlc:
	sqlc generate

# Builda a imagem única (multi-stage) que roda em dev e em produção (§5d).
docker-build:
	docker build -t jus-assessoria:local .

# Sobe a stack local completa (postgres + redis + api + workers + scheduler).
up:
	docker compose up -d --build

# Derruba a stack local.
down:
	docker compose down

# Sobe só as dependências efêmeras (postgres + redis em tmpfs) para integração/e2e.
itest-db:
	docker compose -f docker-compose.test.yml up -d
