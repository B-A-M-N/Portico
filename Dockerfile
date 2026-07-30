# Multi-stage build for minimal distroless image
# Stage 1: Build the binary
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git make gcc musl-dev sqlite-dev

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN make build

# Stage 2: Create minimal runtime image
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /src/portico /usr/bin/portico
COPY --from=builder /src/LICENSE /licenses/LICENSE

USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/portico"]