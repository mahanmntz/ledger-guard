# ==========================================
# Stage 1: Build Environment
# ==========================================
FROM golang:alpine AS builder

WORKDIR /app

# Cache dependencies before copying full source
COPY go.mod go.sum ./
RUN go mod download

# Copy application source code
COPY . .

# Build a statically linked, stripped binary without CGO
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -o /app/ledger-guard .

# ==========================================
# Stage 2: Minimal Production Runtime
# ==========================================
FROM alpine:3.20

# Install root CA certificates for secure outgoing network requests
RUN apk --no-cache add ca-certificates

WORKDIR /app

# Copy only the compiled binary from builder
COPY --from=builder /app/ledger-guard .

EXPOSE 8080

ENTRYPOINT ["./ledger-guard"]