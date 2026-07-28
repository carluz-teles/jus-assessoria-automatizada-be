.PHONY: build test tidy sqlc up down

# Green gate: compila tudo.
build:
	go build ./...

# Green gate: roda todos os testes.
test:
	go test ./...

# Sincroniza go.mod/go.sum.
tidy:
	go mod tidy

# Gera código a partir do SQL (schema em migrations/, queries por slice).
sqlc:
	sqlc generate

# Sobe as dependências locais (postgres + redis + app). Placeholder: o
# docker-compose.yml ainda não existe nesta fundação.
up:
	docker compose up -d

# Derruba a stack local.
down:
	docker compose down
