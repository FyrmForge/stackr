# stackr-observe: the official VictoriaMetrics and VictoriaLogs single-node
# binaries, copied from their release images (pinned by index digest), under
# stackr's small supervisor. Bump a version by its tag and digest together.
FROM victoriametrics/victoria-metrics:v1.154.1@sha256:c7884f32920dd45a9d86542a717227e63af03e060e53bbf8031e6e6fd0e1e05d AS vm
FROM victoriametrics/victoria-logs:v1.53.0@sha256:251121fa882af99b95ba0c230a4a2f412ea602d2698c64a96c58dc9842bb755d AS vl

FROM golang:1.27.3-alpine AS builder
WORKDIR /app
COPY go.mod ./
COPY cmd/stackr-observe ./cmd/stackr-observe
RUN CGO_ENABLED=0 go build -o /stackr-observe ./cmd/stackr-observe

FROM scratch
LABEL org.opencontainers.image.source=https://github.com/FyrmForge/stackr
# stackr-install uninstall --purge finds stackr's images by this label.
LABEL stackr.image=observe
COPY --from=vm /victoria-metrics-prod /victoria-metrics-prod
COPY --from=vl /victoria-logs-prod /victoria-logs-prod
COPY --from=builder /stackr-observe /stackr-observe
ENTRYPOINT ["/stackr-observe"]
