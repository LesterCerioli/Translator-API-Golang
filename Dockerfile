# Stage 1: Build the application
FROM golang:1.24 AS builder

# Set the working directory inside the container
WORKDIR /app

# Configure permissions for the working directory
RUN mkdir -p /app && chmod -R 777 /app

# Copy go.mod and go.sum files first for better layer caching
COPY go.mod go.sum ./

# Install dependencies
RUN go mod download


# Copy the rest of the application source code
COPY . .


# Build the application binary
RUN go build -o app ./cmd

# Stage 2: Run the application in a minimal image
FROM debian:bookworm-slim

# Set the working directory and configure permissions
WORKDIR /app
RUN mkdir -p /app && chmod -R 777 /app

# Install CA certificates (needed for HTTPS requests)
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

# Copy the application binary from the builder stage
COPY --from=builder /app/app .


# Expose the application port
EXPOSE 3080

# Declare environment variables (values will be set at runtime)
ENV DEEPSEEK_API_KEY="" \
    DEEPSEEK_API_URL="https://api.deepseek.com/v1" \
    DEEPSEEK_API_TIMEOUT="10s" \
    DEEPSEEK_API_MAX_RETRIES="3" \
    DEEPSEEK_API_RETRY_DELAY="2s"
    

# Health check (optional but recommended)
HEALTHCHECK --interval=30s --timeout=3s \
  CMD curl -f http://localhost:3080/health || exit 1

# Entry point setup
COPY ./docker/api/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh && \
    chmod +x /app/app

ENTRYPOINT ["/entrypoint.sh"]