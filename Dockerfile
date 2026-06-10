# ── Stage 1: build ───────────────────────────────────────────────────────────
FROM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Cache module downloads before copying source so that source-only changes
# do not invalidate this layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0  → pure-Go binary, no libc dependency (required for distroless)
# -trimpath      → strip local file paths from stack traces (reproducible builds)
# -ldflags -w -s → remove DWARF debug info and symbol table (~30% smaller binary)
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -trimpath -ldflags="-w -s" -o manager cmd/main.go

# ── Stage 2: runtime ─────────────────────────────────────────────────────────
# distroless/static:nonroot contains only CA certificates and timezone data.
# No shell, no package manager — minimal attack surface.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
# UID 65532 is the nonroot user baked into the distroless image.
USER 65532:65532

ENTRYPOINT ["/manager"]
