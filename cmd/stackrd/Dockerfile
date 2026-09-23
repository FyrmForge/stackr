# syntax=docker/dockerfile:1

ARG GO_VERSION=1.27.1
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build

ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev

RUN apk add --no-cache git nodejs npm
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY frontend/package.json frontend/package-lock.json ./frontend/
RUN cd frontend && npm ci

COPY . .

RUN TEMPL_VERSION="$(go list -m -f '{{.Version}}' github.com/a-h/templ)" && \
    HAMR_VERSION="$(go list -m -f '{{.Version}}' github.com/FyrmForge/hamr)" && \
    go install github.com/a-h/templ/cmd/templ@"${TEMPL_VERSION}" && \
    go install github.com/FyrmForge/hamr/cmd/hamr@"${HAMR_VERSION}"

RUN templ generate && \
    cd frontend && npm run css:build && cd .. && \
    hamr gen static

# Generate architecture-independent pages with a build-platform binary. This
# keeps cross-platform image builds from executing the target binary.
RUN CGO_ENABLED=0 go build -trimpath -o /tmp/stackrd-generate ./cmd/stackrd && \
    /tmp/stackrd-generate --generate

RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/stackrd ./cmd/stackrd && \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath \
    -ldflags "-s -w -X github.com/FyrmForge/stackr/internal/cli/cmd.version=${VERSION}" \
    -o /out/stackr ./cmd/stackr

FROM alpine:3.21

# This image runs the panel, node agent, volume tools and move helpers.
RUN apk add --no-cache ca-certificates tzdata git openssh-client rsync docker-cli docker-cli-compose docker-cli-buildx
WORKDIR /app
COPY --from=build /out/stackrd ./stackrd
COPY --from=build /out/stackr ./stackr
COPY --from=build /src/frontend/static ./frontend/static
COPY --from=build /src/frontend/dist ./frontend/dist
COPY --from=build /src/generated ./generated
EXPOSE 8080
ENTRYPOINT ["/app/stackrd"]
