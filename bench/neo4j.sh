#!/bin/sh
# Starts or stops a throwaway Neo4j Community server for the benchmarks.
#
#   bench/neo4j.sh start DIR    create (first time) and start a server in DIR
#   bench/neo4j.sh stop DIR     stop it; delete DIR afterwards to remove it
#
# Then: export CHEESEDB_NEO4J=bolt://127.0.0.1:7699
#
# Needs: brew install neo4j (it brings Java 21). Everything lives in DIR; it
# never touches Neo4j's own Homebrew data folders, and it is not a login
# service. It listens only on 127.0.0.1, with authentication off, so use it
# only on a machine you control. Memory is set explicitly so runs can be
# repeated: a 2 GB heap and a 4 GB page cache hold the 1M-memory dataset.
#
# Durability: Neo4j forces its transaction log to disk before it
# acknowledges a commit. There is no setting to turn that off; BENCHMARKS.md
# checks that a commit costs a full disk flush, as it does for the others.
set -eu
N=/opt/homebrew/opt/neo4j
cmd=$1
dir=$2
export JAVA_HOME=/opt/homebrew/opt/openjdk@21/libexec/openjdk.jdk/Contents/Home
export NEO4J_CONF="$dir/conf"

case "$cmd" in
start)
	if [ ! -d "$dir/conf" ]; then
		mkdir -p "$dir/conf" "$dir/data" "$dir/logs" "$dir/run"
		cp "$N"/libexec/conf/* "$dir/conf/"
		# Drop the Homebrew folders and the settings we set below.
		grep -v -E '^(server\.directories\.(data|logs|run)|server\.bolt\.listen_address|server\.http\.enabled|server\.https\.enabled|dbms\.security\.auth_enabled|server\.memory\.(heap|pagecache))' \
			"$N/libexec/conf/neo4j.conf" >"$dir/conf/neo4j.conf"
		cat >>"$dir/conf/neo4j.conf" <<EOF

server.directories.data=$dir/data
server.directories.logs=$dir/logs
server.directories.run=$dir/run
server.bolt.listen_address=127.0.0.1:7699
server.http.enabled=false
server.https.enabled=false
dbms.security.auth_enabled=false
server.memory.heap.initial_size=2g
server.memory.heap.max_size=2g
server.memory.pagecache.size=4g
EOF
	fi
	"$N/bin/neo4j" start
	i=0
	until /opt/homebrew/bin/cypher-shell -a bolt://127.0.0.1:7699 "RETURN 1" >/dev/null 2>&1; do
		i=$((i + 1))
		[ "$i" -gt 90 ] && { echo "Neo4j did not start; see $dir/logs" >&2; exit 1; }
		sleep 1
	done
	echo "Neo4j ready on bolt://127.0.0.1:7699"
	;;
stop)
	"$N/bin/neo4j" stop
	;;
*)
	echo "usage: $0 start|stop DIR" >&2
	exit 2
	;;
esac
