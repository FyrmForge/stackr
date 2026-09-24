FROM golang:1.27.1-alpine AS builder

ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /stackrd ./cmd/stackrd

FROM alpine:3.21
RUN apk --no-cache add ca-certificates
COPY --from=builder /stackrd /stackrd
COPY --from=builder /app/ui /ui

# SQLite database file directory — mount a volume here for persistence.
RUN mkdir -p /data
VOLUME ["/data"]
ENV DATABASE_PATH=/data/stackr.db

EXPOSE 8080
ENTRYPOINT ["/stackrd"]
