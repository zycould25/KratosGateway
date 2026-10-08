#!/bin/sh
set -eu

if [ "$(id -u)" = 0 ]; then
  mkdir -p "$PGDATA"
  chown -R postgres:postgres "$PGDATA"
  exec gosu postgres /bin/sh "$0" "$@"
fi

mkdir -p "$PGDATA"
chmod 700 "$PGDATA"
if [ ! -s "$PGDATA/PG_VERSION" ]; then
  if [ "$(find "$PGDATA" -mindepth 1 -maxdepth 1 | wc -l)" -ne 0 ]; then
    echo "PGDATA contains an incomplete or unrelated cluster; inspect it before retrying" >&2
    exit 1
  fi
  export PGPASSWORD="$REPLICATION_PASSWORD"
  pg_basebackup -h "$PG_PRIMARY_HOST" -p "${PG_PRIMARY_PORT:-5432}" -U replicator -D "$PGDATA" -Fp -Xs -P -R
fi

export PGPASSWORD="$REPLICATION_PASSWORD"
exec postgres -c listen_addresses='*' -c hot_standby=on
