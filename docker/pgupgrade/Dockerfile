# stackr-pgupgrade: the official postgres image of the newest major plus the
# server binaries of every older major stackr runs (14 to 17, from the PGDG
# apt repo the official image already uses), so one container can run
# pg_upgrade, or a dump with the newer pg_dump, between any two. No code of
# ours: stackr passes the scripts at run time. Rebuilt weekly for Debian's
# fixes (.github/workflows/pgupgrade-image.yml). Bump the base by tag and
# digest together, and add a major here when stackr adds one.
FROM postgres:18.6@sha256:e3d24f6906d0760bf4da0dae872415391ededefad23fd471c47dc53b0ddf3c42

# No default cluster: the packages would make a "main" one under /var/lib.
RUN mkdir -p /etc/postgresql-common \
 && echo "create_main_cluster = false" > /etc/postgresql-common/createcluster.conf \
 && apt-get update \
 && apt-get upgrade -y --no-install-recommends \
 && apt-get install -y --no-install-recommends \
      postgresql-14 postgresql-15 postgresql-16 postgresql-17 \
 && for v in 14 15 16 17 18; do test -x /usr/lib/postgresql/$v/bin/pg_upgrade || exit 1; done \
 && rm -rf /var/lib/apt/lists/* \
# gosu is a Go binary with a stale stdlib (Trivy CVEs); the script uses setpriv.
 && rm -f /usr/local/bin/gosu

LABEL org.opencontainers.image.source=https://github.com/FyrmForge/stackr
# stackr-install uninstall --purge finds stackr's images by this label.
LABEL stackr.image=pgupgrade
