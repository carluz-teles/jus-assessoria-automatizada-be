.PHONY: build test test-integration tidy sqlc docker-build up down up-full down-full up-tunnel down-tunnel itest-db

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

# Builda as SEIS imagens lean (uma por serviço) a partir do único Dockerfile
# parametrizado por SVC (§5d). Cada imagem carrega só o seu binário; o ENTRYPOINT
# é o próprio binário (sem command override — o provider da Railway não seta um).
docker-build:
	@for svc in api worker-ingestao worker-documents worker-ai worker-outbox-relay scheduler; do \
		echo "==> building jus-$$svc:local"; \
		docker build --build-arg SVC=$$svc -t jus-$$svc:local . || exit 1; \
	done

# Sobe a stack local completa (postgres + redis + api + workers + scheduler).
up:
	docker compose up -d --build

# Derruba a stack local.
down:
	docker compose down

# Sobe a stack completa + FE (repo irmão ../jus-assessoria-automatizada-fe — ver
# docker-compose.fe.yml). Só dev local; nenhuma CI depende disto.
up-full:
	docker compose -f docker-compose.yml -f docker-compose.fe.yml up -d --build

down-full:
	docker compose -f docker-compose.yml -f docker-compose.fe.yml down

# Sobe a stack completa + FE + túnel público (cloudflared, sem conta) na frente
# do api — necessário pro webhook do Clerk (organization.created) alcançar
# localhost. Ver docker-compose.tunnel.yml pro passo de cadastro no Clerk Dashboard.
up-tunnel:
	docker compose -f docker-compose.yml -f docker-compose.fe.yml -f docker-compose.tunnel.yml up -d --build

down-tunnel:
	docker compose -f docker-compose.yml -f docker-compose.fe.yml -f docker-compose.tunnel.yml down

# Sobe só as dependências efêmeras (postgres + redis em tmpfs) para integração/e2e.
itest-db:
	docker compose -f docker-compose.test.yml up -d
