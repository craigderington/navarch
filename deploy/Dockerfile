# Toolchain is 1.25 (not 1.23): the Docker Engine SDK the agent uses pulls an
# OpenTelemetry/grpc stack that requires go >= 1.25. Bumped deliberately and in
# step with the go.mod directive — a real runtime dependency, unlike the earlier
# test-only go-internal pin.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
WORKDIR /src

# Stamped into internal/version so a deployed control plane can answer "which
# build is this" on /healthz, and an agent reports something true at
# registration. Unset, they default to "dev" — a binary built from a checkout
# should say so rather than claim to be a release.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
# Provided by buildx when cross-building; defaulted so a plain `docker build`
# still works.
ARG TARGETOS=linux
ARG TARGETARCH

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV MOD=github.com/craigderington/navarch
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X $MOD/internal/version.Version=$VERSION -X $MOD/internal/version.Commit=$COMMIT -X $MOD/internal/version.Date=$BUILD_DATE" \
    -o /out/composectl-controlplane ./cmd/controlplane
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X $MOD/internal/version.Version=$VERSION -X $MOD/internal/version.Commit=$COMMIT -X $MOD/internal/version.Date=$BUILD_DATE" \
    -o /out/composectl-agent ./cmd/agent
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X $MOD/internal/version.Version=$VERSION -X $MOD/internal/version.Commit=$COMMIT -X $MOD/internal/version.Date=$BUILD_DATE" \
    -o /out/navarch-web ./cmd/navarch-web

# --- control plane: HTTP API, runs unprivileged --------------------------
FROM gcr.io/distroless/static-debian12:nonroot AS controlplane
COPY --from=build /out/composectl-controlplane /composectl-controlplane
USER nonroot:nonroot
EXPOSE 8417
ENTRYPOINT ["/composectl-controlplane"]

# --- node agent: needs the host docker socket, so runs as root -----------
# The socket is root:docker 660; running as root is the simplest dev-stack way
# to reach it. Production would add the agent to the docker group instead.
FROM gcr.io/distroless/static-debian12 AS agent
COPY --from=build /out/composectl-agent /composectl-agent
ENTRYPOINT ["/composectl-agent"]

# --- web console: renders HTML, holds operator sessions, talks only to the API
FROM gcr.io/distroless/static-debian12:nonroot AS web
COPY --from=build /out/navarch-web /navarch-web
USER nonroot:nonroot
EXPOSE 8418
ENTRYPOINT ["/navarch-web"]
