#!/bin/sh
# Starts or stops a throwaway PostgreSQL server for the benchmarks.
#
#   bench/pg.sh start DIR    create (first time) and start a server in DIR
#   bench/pg.sh stop DIR     stop it; delete DIR afterwards to remove it
#
# Then: export CHEESEDB_PG="host=127.0.0.1 port=54329 user=$USER"
#
# It listens only on 127.0.0.1 (no Unix socket: socket paths have a ~100
# character limit that deep temp folders exceed). Durability matches
# cheesedb and the SQLite setup: fsync on, every commit synchronous, and
# fsync_writethrough, which is F_FULLFSYNC on macOS.
set -eu
PG=/opt/homebrew/opt/postgresql@17/bin
cmd=$1
dir=$2

case "$cmd" in
start)
	if [ ! -d "$dir/data" ]; then
		"$PG/initdb" -D "$dir/data" -A trust -U "$USER" >/dev/null
	fi
	"$PG/pg_ctl" -D "$dir/data" -l "$dir/server.log" -w start -o \
		"-p 54329 -c listen_addresses=127.0.0.1 -c unix_socket_directories='' \
		 -c fsync=on -c synchronous_commit=on -c full_page_writes=on \
		 -c wal_sync_method=fsync_writethrough"
	;;
stop)
	"$PG/pg_ctl" -D "$dir/data" -m fast stop
	;;
*)
	echo "usage: $0 start|stop DIR" >&2
	exit 2
	;;
esac
