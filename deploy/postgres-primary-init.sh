#!/bin/sh
set -eu

case "$REPLICATION_CIDR" in
  *[!0-9a-fA-F.:/]*|'') echo "invalid REPLICATION_CIDR" >&2; exit 1 ;;
esac

psql -v ON_ERROR_STOP=1 -v replication_password="$REPLICATION_PASSWORD" -v probe_password="$POSTGRES_PROBE_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
CREATE ROLE replicator WITH LOGIN REPLICATION PASSWORD :'replication_password';
CREATE ROLE kratos_probe WITH LOGIN PASSWORD :'probe_password';
SQL

printf 'host replication replicator %s scram-sha-256\n' "$REPLICATION_CIDR" >> "$PGDATA/pg_hba.conf"
