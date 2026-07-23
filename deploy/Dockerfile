# Toolchain is 1.25 (not 1.23): the Docker Engine SDK the agent uses pulls an
# OpenTelemetry/grpc stack that requires go >= 1.25. Bumped deliberately and in
# step with the go.mod directive — a real runtime dependency, unlike the earlier
# test-only go-internal pin.
FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /out/composectl-controlplane ./cmd/controlplane
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" \
    -o /out/composectl-agent ./cmd/agent

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
