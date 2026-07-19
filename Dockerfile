# ---- Build stage ----
FROM golang:1.25-alpine AS builder

WORKDIR /app

# Cache dependency downloads separately from source changes
COPY go.mod go.sum* ./
RUN go mod download

COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bot main.go

# ---- Run stage ----
FROM alpine:3.20

# Certs are needed for TLS to Telegram's API and MongoDB Atlas (mongodb+srv)
RUN apk add --no-cache ca-certificates && \
    adduser -D -u 10001 botuser

WORKDIR /app
COPY --from=builder /bot /app/bot

USER botuser

ENTRYPOINT ["/app/bot"]
