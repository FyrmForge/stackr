# Helper image for live env moves: an rsync daemon on the target node and an
# rsync client on the source node, one volume mounted at /data on each side.
#   CMD ["daemon"] + env RSYNC_ALLOW (the one address allowed in): the daemon
#   any other CMD runs as is: rsync -aH ... /data/ rsync://host:port/data/
FROM alpine:3
RUN apk add --no-cache rsync
COPY entry.sh /entry.sh
RUN chmod +x /entry.sh
EXPOSE 873
ENTRYPOINT ["/entry.sh"]
