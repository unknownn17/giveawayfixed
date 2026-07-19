FROM golang:1.25.5-alpine AS builder

WORKDIR /app

# Use module download caching
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# Statically build for linux
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w" -o /app/main .

FROM alpine:3.18
WORKDIR /app
COPY --from=builder /app/main /app/main
CMD ["/app/main"]