# Build stage
FROM golang:1.21-alpine AS builder

LABEL author="Abdulrahman Jimoh"

WORKDIR /app

RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /app/resource-watcher

# Final stage
FROM alpine:3.19

RUN adduser -D -u 1000 appuser

RUN mkdir -p /app /etc/resource-watcher/secrets /tmp && \
    chown -R appuser:appuser /app /etc/resource-watcher /tmp

WORKDIR /app

COPY --from=builder /app/resource-watcher .

RUN chown appuser:appuser /app/resource-watcher

USER appuser

ENTRYPOINT ["/app/resource-watcher"] 
