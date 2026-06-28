# Multi-stage build: compile a static, dependency-free router binary, then ship
# it on a minimal distroless base. The result is a single self-contained image
# that mirrors the standalone binaries GoReleaser publishes (see .goreleaser.yaml).

# ---- build ----
FROM golang:1.23-alpine AS build

WORKDIR /src

# Cache module downloads separately from the source for faster rebuilds.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 produces a fully static binary so it runs on the distroless
# (and scratch-like) base with no libc. Build metadata is stamped to match the
# --version output GoReleaser produces for tagged releases.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/router ./cmd/router

# ---- runtime ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/router /usr/local/bin/router

# Consumer + operator endpoints are served here (ADR-0011).
EXPOSE 8080

# Config is mounted at /etc/router/config.yaml (Secret/ConfigMap) by the
# deployment; --config is required (ADR-0010).
ENTRYPOINT ["/usr/local/bin/router"]
CMD ["--config", "/etc/router/config.yaml"]
