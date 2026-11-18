FROM golang:1.27.4-alpine AS builder

ARG VERSION=dev

WORKDIR /app
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags release -ldflags "-X main.version=${VERSION}" -o /stackrd ./cmd/stackrd
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /stackr ./cmd/stkr
RUN CGO_ENABLED=0 go build -ldflags "-X main.version=${VERSION}" -o /stackr-install ./cmd/stackr-install

FROM alpine:3.24
# stackr-install uninstall --purge finds stackr's images by this label,
# an old release's too once a newer one took its tag.
LABEL stackr.image=stackrd
# iptables for the VIP rules, git for clones, docker-cli + buildx for builds,
# conntrack-tools for tile traffic on kernels without /proc/net/nf_conntrack,
# iproute2 because busybox ip refuses routing tables above 255 (exit links).
# ponytail: alpine's iptables is the nft backend; a host whose docker still
# writes legacy iptables needs iptables-legacy here instead.
# xfsprogs + xfsprogs-extra (mkfs.xfs, xfs_growfs, xfs_quota) and util-linux (lsblk, wipefs,
# nsenter) for xfs node disks; the host helper runs them (internal/hostdisk).
RUN apk --no-cache add ca-certificates iptables git docker-cli docker-cli-buildx conntrack-tools iproute2 xfsprogs xfsprogs-extra util-linux
# Pull the base's security fixes (libcrypto) that landed after the alpine tag.
RUN apk upgrade --no-cache
COPY --from=builder /stackrd /stackrd
# The CLI is built from cmd/stkr and stays at /stackr in the image: installed
# wrappers (stkr, and the older stackr) exec /stackr in the container.
COPY --from=builder /stackr /stackr
# The installer, next to stackrd: a worker upgrade swaps both (worker-swap).
COPY --from=builder /stackr-install /stackr-install
COPY --from=builder /app/ui /ui

# No VOLUME: the install spec binds the data dir; an anonymous volume
# would swallow the database if the spec ever lost DATABASE_PATH.

EXPOSE 8080
ENTRYPOINT ["/stackrd"]
