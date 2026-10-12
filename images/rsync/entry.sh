#!/bin/sh
set -e
if [ "$1" = daemon ]; then
	cat > /tmp/rsyncd.conf <<CONF
use chroot = no
numeric ids = yes
[data]
	path = /data
	read only = no
	uid = 0
	gid = 0
	hosts allow = ${RSYNC_ALLOW:?RSYNC_ALLOW unset}
	hosts deny = *
CONF
	exec rsync --daemon --no-detach --config=/tmp/rsyncd.conf
fi
exec "$@"
