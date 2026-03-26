FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /llm-proxy .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates
COPY --from=builder /llm-proxy /usr/local/bin/llm-proxy

EXPOSE 4000

ENTRYPOINT ["llm-proxy"]
