FROM golang:1.27.1-alpine AS builder

ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /stackrd ./cmd/stackrd
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /stackr ./cmd/stackr

FROM alpine:3.21
# iptables for the VIP rules, git for clones, docker-cli + buildx for builds.
# ponytail: alpine's iptables is the nft backend; a host whose docker still
# writes legacy iptables needs iptables-legacy here instead.
RUN apk --no-cache add ca-certificates iptables git docker-cli docker-cli-buildx
COPY --from=builder /stackrd /stackrd
# The CLI, reached through the installer's /usr/local/bin/stackr wrapper.
COPY --from=builder /stackr /stackr
COPY --from=builder /app/ui /ui

# No VOLUME: the install spec binds the data dir; an anonymous volume
# would swallow the database if the spec ever lost DATABASE_PATH.

EXPOSE 8080
ENTRYPOINT ["/stackrd"]
