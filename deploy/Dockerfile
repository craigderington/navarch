FROM golang:1.23-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o /out/composectl-controlplane \
    ./cmd/controlplane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/composectl-controlplane /composectl-controlplane
USER nonroot:nonroot
EXPOSE 8417
ENTRYPOINT ["/composectl-controlplane"]
