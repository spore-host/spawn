# Multi-stage build for spawn and truffle CLI tools
# Produces minimal container with spawn, truffle binaries and AWS tools

FROM golang:1.26-alpine AS builder

# Use Docker's automatic platform ARGs for multi-platform builds
ARG TARGETARCH

WORKDIR /build

# Copy shared dependencies
COPY pkg/i18n pkg/i18n/
COPY pkg/pricing pkg/pricing/
COPY pkg/catalog pkg/catalog/

# Build spawn
COPY spawn/go.mod spawn/go.sum spawn/
WORKDIR /build/spawn
RUN go mod download
COPY spawn/ .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o spawn main.go

# Build truffle
WORKDIR /build
COPY truffle/go.mod truffle/go.sum truffle/
WORKDIR /build/truffle
RUN go mod download
COPY truffle/ .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} go build -ldflags="-w -s" -o truffle main.go

# Final stage - minimal runtime image
FROM alpine:latest

LABEL maintainer="Scott Friedman <scott@example.com>"
LABEL description="spawn and truffle - AWS EC2 management and discovery tools"
LABEL version="0.12.0"

# Install runtime dependencies
RUN apk --no-cache add \
    ca-certificates \
    aws-cli \
    openssh-client \
    bash \
    jq \
    curl

# Create non-root user for runtime
RUN adduser -D -h /home/spawn spawn

# Copy binaries from builder
COPY --from=builder /build/spawn/spawn /usr/local/bin/spawn
COPY --from=builder /build/truffle/truffle /usr/local/bin/truffle

# Verify binaries work
RUN spawn --version || spawn --help
RUN truffle --version || truffle --help

WORKDIR /workspace
RUN chown spawn:spawn /workspace

USER spawn

ENTRYPOINT ["spawn"]
CMD ["--help"]
