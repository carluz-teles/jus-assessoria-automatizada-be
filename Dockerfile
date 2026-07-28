# syntax=docker/dockerfile:1

# ONE Dockerfile, parameterized by service (docs/erd-backend.md §5d). The build
# ARG `SVC` selects which cmd/ to compile, so this single file produces SIX lean
# per-service images (jus-<svc>:local) — each carrying ONLY its own binary and an
# ENTRYPOINT that IS that binary. No command override is ever needed, which is the
# whole point: the Railway community provider's railway_service has no
# start_command, so the image must be self-contained per service.

# ---- build stage: compile the one selected binary, statically ----------------
FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first: this layer stays cached until go.mod/go.sum change, so
# source edits do not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

# Then the source. `SVC` picks the cmd/ to build (api, worker-ingestao,
# worker-documents, worker-ai, worker-outbox-relay, scheduler). CGO off + fully
# static so the binary runs on distroless static (no libc). -trimpath strips
# local paths; -s -w drop debug/symbol tables.
COPY . .
ARG SVC=api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${SVC}

# ---- final stage: minimal, non-root, no shell --------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

# Just this service's binary. The migrations ride along too: only the api applies
# them at boot, but the copy is cheap and harmless (keeps them available for
# inspection/tools per §5d and keeps every image built the same way).
COPY --from=build /out/app /app/app
COPY migrations/ /app/migrations/

USER nonroot:nonroot

# The ENTRYPOINT is the binary itself — no per-service command to override.
ENTRYPOINT ["/app/app"]
