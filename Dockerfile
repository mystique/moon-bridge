FROM golang:alpine AS builder

ENV GOPROXY=https://goproxy.cn,direct

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/moonbridge ./cmd/moonbridge

FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -s /sbin/nologin nonroot

WORKDIR /app

COPY --from=builder /out/moonbridge /app/moonbridge

USER nonroot:nonroot
ENTRYPOINT ["/app/moonbridge"]
CMD ["-config", "/config/config.yml"]
