# syntax=docker/dockerfile:1

# ONE Dockerfile, parameterized by service (docs/erd-backend.md §5d). The build
# ARG `SVC` selects which cmd/ to compile, so this single file produces SIX lean
# per-service images (jus-<svc>:local) — each carrying ONLY its own binary and an
# ENTRYPOINT that IS that binary. No command override is ever needed, which is the
# whole point: the Railway community provider's railway_service has no
# start_command, so the image must be self-contained per service.

# ---- build stage: compile the one selected binary, statically ----------------
FROM golang:1.26-alpine AS build
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

# ---- runtime-ocr stage: for worker-documents (needs the OCR toolchain) -------
# The default distroless/static image has no shell and can't run external binaries,
# so worker-documents — which shells out to pdftoppm + tesseract for deterministic,
# free OCR — targets THIS stage instead (cd.yml sets `target: runtime-ocr` only for
# that svc). Debian slim + poppler-utils (pdftoppm) + tesseract-ocr + the Portuguese
# language pack (tesseract-ocr-por). Same binary/migrations/ENTRYPOINT shape and a
# non-root user, so nothing else about how the service runs changes.
#
# ca-certificates + tzdata are baked into the distroless static image but NOT into
# debian-slim — without ca-certificates every outbound TLS (OTLP metrics/traces, the
# Voyage embeddings API, the R2 presigned GET/PUT) fails with "x509: certificate signed
# by unknown authority". They restore parity with the distroless runtime.
FROM debian:bookworm-slim AS runtime-ocr
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata \
        tesseract-ocr tesseract-ocr-por poppler-utils \
    && rm -rf /var/lib/apt/lists/*
RUN useradd --system --no-create-home --uid 10001 appuser
WORKDIR /app
COPY --from=build /out/app /app/app
COPY migrations/ /app/migrations/
USER appuser
ENTRYPOINT ["/app/app"]

# ---- runtime-chromium stage: for `api` (needs Chromium pra renderer HTML→PDF) ----
# O renderer PDF do editor rico (Fase C — pdfgen/html.go) chama Chromium
# headless via chromedp. Distroless não tem executáveis externos, então o
# api roda em debian-slim + chromium + fontes + ca-certs. ~200MB a mais que
# distroless mas necessário — sem isso, o sign() falha.
#
# fonts-liberation: garante que a família "Liberation Serif" resolva mesmo
# quando o CSS @font-face inline falhar (fallback seguro).
FROM debian:bookworm-slim AS runtime-chromium
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates tzdata \
        chromium fonts-liberation \
    && rm -rf /var/lib/apt/lists/*
# chromedp usa "chromium" ou "chromium-browser" no PATH; deixamos ambos.
RUN ln -sf /usr/bin/chromium /usr/bin/chromium-browser || true
RUN useradd --system --no-create-home --uid 10001 appuser
WORKDIR /app
COPY --from=build /out/app /app/app
COPY migrations/ /app/migrations/
USER appuser
ENTRYPOINT ["/app/app"]

# ---- runtime stage (DEFAULT): minimal, non-root, no shell --------------------
# LAST stage = the implicit target for a target-less build, so the other five
# services (and any `docker build` without --target) keep the same lean distroless
# static image they had before. Only worker-documents overrides to runtime-ocr,
# api overrides to runtime-chromium.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime
WORKDIR /app

# Just this service's binary. The migrations ride along too: only the api applies
# them at boot, but the copy is cheap and harmless (keeps them available for
# inspection/tools per §5d and keeps every image built the same way).
COPY --from=build /out/app /app/app
COPY migrations/ /app/migrations/

USER nonroot:nonroot

# The ENTRYPOINT is the binary itself — no per-service command to override.
ENTRYPOINT ["/app/app"]
