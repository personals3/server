# =============================================================================
# Build stage — produces a static binary
# =============================================================================
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Cache deps separately from source
COPY go.mod ./
COPY go.sum* ./

# Build — `go mod tidy` populates go.sum if missing, then we build
COPY . .
RUN go mod tidy && \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=linux go build \
        -trimpath -ldflags="-s -w" \
        -o /out/cleaner ./cmd/cleaner

# =============================================================================
# Runtime stage — minimal image
# =============================================================================
FROM alpine:3.20

# ffmpeg pulls in ffprobe, which the transcode-enqueue path uses to detect
# the source video's resolution so it can build a per-source quality ladder
# (only enqueues rungs <= the source height).
RUN apk add --no-cache ca-certificates tzdata wget ffmpeg

COPY --from=builder /out/server  /usr/local/bin/server
COPY --from=builder /out/cleaner /usr/local/bin/cleaner

# No default USER — compose sets `user: "${UID}:${GID}"` to match the host
# owner of the bind-mounted /storage volume. Without this, the container
# can't write to host-owned directories.

EXPOSE 3000

ENTRYPOINT ["/usr/local/bin/server"]
