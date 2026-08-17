# Multi-stage build for minimal distroless image
# Stage 1: Build the binary (glibc-compatible)
FROM golang:1.25-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    gcc \
    libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 make build

# Stage 2: Create minimal runtime image (glibc-based for CGO sqlite3)
FROM gcr.io/distroless/base-debian12:nonroot

COPY --from=builder /src/portico /usr/bin/portico
COPY --from=builder /src/LICENSE /licenses/LICENSE

USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/portico"]
