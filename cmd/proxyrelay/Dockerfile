# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.1
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/proxyrelay ./cmd/proxyrelay
COPY internal/proxyrelay ./internal/proxyrelay
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags "-s -w" -o /out/proxyrelay ./cmd/proxyrelay

FROM scratch
COPY --from=build /out/proxyrelay /proxyrelay
USER 65532:65532
ENTRYPOINT ["/proxyrelay"]
