# syntax=docker/dockerfile:1

# Builder: matches go.mod (go 1.26.4).
FROM golang:1.26 AS builder
WORKDIR /src

ARG VERSION=dev
ARG GIT_COMMIT=unknown
ARG BUILD_DATE=unknown
ARG MODULE=github.com/complytime-labs/crosscodex

COPY go.mod go.sum ./
RUN go mod download

# Copy proto definitions and generate code.
COPY api/proto/ ./api/proto/
COPY internal/testgrpc/proto/ ./internal/testgrpc/proto/
WORKDIR /src/api/proto
RUN go tool buf generate
WORKDIR /src/internal/testgrpc/proto
RUN go tool buf generate
WORKDIR /src

COPY . .

ENV CGO_ENABLED=0
RUN LDFLAGS="-s -w -X ${MODULE}/internal/version.Version=${VERSION} -X ${MODULE}/internal/version.GitCommit=${GIT_COMMIT} -X ${MODULE}/internal/version.BuildDate=${BUILD_DATE}" && \
    go build -ldflags "${LDFLAGS}" -o /out/crosscodexd ./cmd/crosscodexd && \
    go build -ldflags "${LDFLAGS}" -o /out/crosscodex  ./cmd/crosscodex

# Daemon image.
FROM gcr.io/distroless/static-debian12:nonroot AS crosscodexd
COPY --from=builder /out/crosscodexd /crosscodexd
USER nonroot:nonroot
ENTRYPOINT ["/crosscodexd"]

# CLI image.
FROM gcr.io/distroless/static-debian12:nonroot AS crosscodex-cli
COPY --from=builder /out/crosscodex /crosscodex
USER nonroot:nonroot
ENTRYPOINT ["/crosscodex"]
