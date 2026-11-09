# stackr-pgbouncer: Alpine's own pgbouncer package under a non-root user, no
# code of ours. stackr writes /etc/pgbouncer/pgbouncer.ini and the userlist
# at start and signals a reload. psql and pg_isready are for stackr's admin
# console reads and the health check. Rebuilt weekly for Alpine's fixes
# (.github/workflows/pgbouncer-image.yml).
FROM alpine:3.22

# stackr's config (max_prepared_statements) needs pgbouncer 1.21 or later.
RUN apk add --no-cache pgbouncer postgresql-client \
 && v=$(pgbouncer --version | sed -n 's/^.*version \([0-9]*\)\.\([0-9]*\).*/\1\2/p') \
 && [ "${v:-0}" -ge 121 ] \
 && rm -f /etc/pgbouncer/pgbouncer.ini /etc/pgbouncer/userlist.txt \
 && mkdir -p /var/run/pgbouncer \
 && chown -R pgbouncer:pgbouncer /etc/pgbouncer /var/run/pgbouncer \
 && chmod 750 /etc/pgbouncer

LABEL org.opencontainers.image.source=https://github.com/FyrmForge/stackr
# stackr-install uninstall --purge finds stackr's images by this label.
LABEL stackr.image=pgbouncer

USER pgbouncer
EXPOSE 6432
CMD ["pgbouncer", "/etc/pgbouncer/pgbouncer.ini"]
