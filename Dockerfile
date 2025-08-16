
FROM golang:1.22.2-alpine AS builder


RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .


RUN CGO_ENABLED=0 GOOS=linux go build -o zabbix-media-monitor

FROM alpine:latest

RUN apk add --no-cache ca-certificates tzdata


RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app
COPY --from=builder /app/zabbix-media-monitor .
RUN chown -R appuser:appgroup /app
USER appuser
CMD ["./zabbix-media-monitor"]