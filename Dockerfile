# Stage 1: Build the Go daemon.
# Pin the builder to the native build host ($BUILDPLATFORM) and cross-compile to
# the requested target arch. CGO is disabled, so Go cross-compiles trivially and
# we avoid slow QEMU emulation when producing multi-arch images.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine3.23 AS builder

# libcap provides setcap, used below to grant the ICMP capability to the binary.
RUN apk add --no-cache libcap

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

# Copy sources explicitly to keep the module-download layer cache-stable.
COPY cmd/ ./cmd/
COPY pkg/ ./pkg/

# Compile a statically linked binary (CGO_ENABLED=0 to avoid libc dependencies)
# for the platform buildx is currently targeting (TARGETOS/TARGETARCH are
# injected automatically per --platform entry).
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags="-w -s" -o /app/bin/tgshd cmd/main.go

# Grant CAP_NET_RAW as a file capability so the daemon can open raw ICMP sockets
# (privileged ping) while running as an unprivileged user. This is a plain xattr
# write on the file, independent of the target arch, and is preserved by the
# COPY into the runtime stage below. NET_RAW is in Docker's default capability
# bounding set, so no extra --cap-add is required at runtime.
RUN setcap cap_net_raw+ep /app/bin/tgshd

# Stage 2: Run target.
# Distroless static already ships ca-certificates (needed for the Telegram and
# speedtest HTTPS calls) and a nonroot (65532) user.
FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app
COPY --from=builder /app/bin/tgshd /app/tgshd

USER 65532:65532
ENV APP_ENV=production

ENTRYPOINT ["/app/tgshd"]
