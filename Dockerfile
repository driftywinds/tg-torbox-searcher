# ---- Build Stage ----
FROM golang:1.21-alpine AS builder

WORKDIR /build

# Cache dependencies first (they change less often)
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source
COPY . .

# Build statically-linked binary for the target platform.
# TARGETOS and TARGETARCH are set automatically when building with
# `docker buildx` for multi-platform, or can be passed manually.
ARG TARGETOS TARGETARCH
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} CGO_ENABLED=0 \
    go build -ldflags="-s -w" -o torbox-bot .

# ---- Runtime Stage ----
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

# Default data directory — matches the VOLUME path below so that
# the authorised-users list survives container restarts.
ENV DATA_DIR=/data

VOLUME ["/data"]

WORKDIR /app

COPY --from=builder /build/torbox-bot .

ENTRYPOINT ["./torbox-bot"]
