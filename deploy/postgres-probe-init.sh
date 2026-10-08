#!/bin/sh
# Apply explicitly to an existing cluster. The official init directory runs
# only on a new cluster, so adding an environment variable alone is insufficient.
set -eu
psql -v ON_ERROR_STOP=1 -v probe_password="$POSTGRES_PROBE_PASSWORD" --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'SQL'
SELECT 'CREATE ROLE kratos_probe WITH LOGIN'
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'kratos_probe') \gexec
ALTER ROLE kratos_probe WITH PASSWORD :'probe_password';
SQL
