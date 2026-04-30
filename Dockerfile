FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /ai-proxy .

# ---

FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /ai-proxy /usr/local/bin/ai-proxy

EXPOSE 8080

ENTRYPOINT ["ai-proxy"]
CMD ["-config", "/etc/ai-proxy/config.yaml"]
