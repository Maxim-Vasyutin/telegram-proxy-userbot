# syntax=docker/dockerfile:1
#
# Multi-stage build for the telegram-proxy-userbot.
#
# Stage 1 (builder)   — compiles a static Go binary.
# Stage 2 (dirs)      — prepares /var/lib/userbot with correct ownership.
#                       distroless/static has no shell, so we cannot chown
#                       at runtime; we stage the directory tree here and
#                       COPY it into the final image with --chown.
# Stage 3 (final)     — minimal runtime image (distroless).

# ---------------------------------------------------------------------------
# Stage 1: build static binary
# ---------------------------------------------------------------------------
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Download dependencies first (cached layer)
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-w -s" \
    -o /app/userbot \
    ./cmd/userbot

# ---------------------------------------------------------------------------
# Stage 2: pre-create persistent directories owned by the runtime UID
# ---------------------------------------------------------------------------
# /var/lib/userbot           — root for state files (BoltDB)
# /var/lib/userbot/sessions  — *.session files written by gotd FileStorage
#
# The runtime UID 10001 matches `nonroot` in distroless/static. We create
# the directories here in a stage that has a shell, then COPY them across
# with --chown so the runtime user can write into them.
FROM alpine:3.20 AS dirs
RUN mkdir -p /var/lib/userbot/sessions \
    && chmod 0700 /var/lib/userbot \
    && chmod 0700 /var/lib/userbot/sessions

# ---------------------------------------------------------------------------
# Stage 3: minimal runtime image
# ---------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /app/userbot /app/userbot
COPY --from=dirs --chown=10001:10001 /var/lib/userbot /var/lib/userbot

WORKDIR /app

# Run as non-root (uid:gid 10001:10001 provided by distroless nonroot)
USER 10001:10001

ENTRYPOINT ["/app/userbot"]
