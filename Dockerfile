# syntax=docker/dockerfile:1

# The SAME image runs every service in dev and in prod (docs/erd-backend.md §5d):
# what you build locally is what Railway runs. compose/Railway only override the
# `command` to pick which of the six binaries a given service runs.

# ---- build stage: compile all six binaries, statically -----------------------
FROM golang:1.25-alpine AS build
WORKDIR /src

# Dependencies first: this layer stays cached until go.mod/go.sum change, so
# source edits do not re-download the module graph.
COPY go.mod go.sum ./
RUN go mod download

# Then the source. CGO off + fully static so the binaries run on distroless
# static (no libc). -trimpath strips local paths; -s -w drop debug/symbol tables.
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/...

# ---- final stage: minimal, non-root, no shell --------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

# The six entrypoints, plus the SQL the api applies at boot (the api embeds the
# migrations too — this copy keeps them available for inspection/tools per §5d).
COPY --from=build /out/ /app/
COPY migrations/ /app/migrations/

USER nonroot:nonroot

# api is the default; compose/Railway override `command` per service to run
# scheduler / worker-* off this same image. One image, six start commands.
ENTRYPOINT ["/app/api"]
