#!/usr/bin/env bash
# Run a command with CICADO_TEST_DATABASE_URL pointing at a Postgres.
#
# If CICADO_TEST_DATABASE_URL is already set, this is a no-op wrapper and the
# command runs against whatever you pointed it at — that is the path CI takes,
# where a service container provides the database.
#
# Otherwise it starts a throwaway cluster in a temp directory, hands the command
# a URL for it, and tears the whole thing down on exit. Nothing touches any
# Postgres you may have running locally: distinct data directory, distinct port,
# and a unix socket inside the temp directory.
set -euo pipefail

if [[ $# -eq 0 ]]; then
	echo "usage: $0 <command> [args...]" >&2
	exit 2
fi

if [[ -n "${CICADO_TEST_DATABASE_URL:-}" ]]; then
	echo "with-postgres: using CICADO_TEST_DATABASE_URL from the environment" >&2
	exec "$@"
fi

# Locate the server binaries. They are not on PATH on Debian/Ubuntu, where the
# packaged cluster tooling lives under /usr/lib/postgresql/<version>/bin.
PGBIN=""
if command -v initdb >/dev/null 2>&1 && command -v pg_ctl >/dev/null 2>&1; then
	PGBIN="$(dirname "$(command -v initdb)")"
else
	for candidate in /usr/lib/postgresql/*/bin /usr/pgsql-*/bin /opt/homebrew/opt/postgresql@*/bin; do
		if [[ -x "${candidate}/initdb" ]]; then PGBIN="${candidate}"; fi
	done
fi

if [[ -z "${PGBIN}" ]]; then
	cat >&2 <<'MSG'
with-postgres: no Postgres server binaries found.

Either install a local Postgres, or point the tests at one you already have:

    CICADO_TEST_DATABASE_URL=postgres://user:pass@host:5432/db make test-all
MSG
	exit 1
fi

PGDATA_DIR="$(mktemp -d -t cicado-pg-XXXXXX)"
PGPORT="${CICADO_TEST_PGPORT:-$(( 15432 + RANDOM % 10000 ))}"
LOGFILE="${PGDATA_DIR}/postgres.log"

# Postgres refuses to run as root. When we are root (containers, CI images), run
# the cluster as an unprivileged user and give it ownership of the temp dir.
RUNAS=""
if [[ "$(id -u)" -eq 0 ]]; then
	for u in postgres nobody; do
		if id "$u" >/dev/null 2>&1; then RUNAS="$u"; break; fi
	done
	if [[ -z "${RUNAS}" ]]; then
		echo "with-postgres: running as root and no unprivileged user to drop to" >&2
		exit 1
	fi
	chown -R "${RUNAS}" "${PGDATA_DIR}"
fi

run_pg() {
	if [[ -n "${RUNAS}" ]]; then
		su -s /bin/bash "${RUNAS}" -c "$*"
	else
		bash -c "$*"
	fi
}

cleanup() {
	local status=$?
	if [[ -f "${PGDATA_DIR}/data/postmaster.pid" ]]; then
		run_pg "'${PGBIN}/pg_ctl' -D '${PGDATA_DIR}/data' -m immediate stop" >/dev/null 2>&1 || true
	fi
	if [[ ${status} -ne 0 && -f "${LOGFILE}" ]]; then
		echo "--- postgres log ---" >&2
		tail -40 "${LOGFILE}" >&2 || true
	fi
	rm -rf "${PGDATA_DIR}"
	exit ${status}
}
# EXIT/INT/TERM, not KILL — SIGKILL cannot be trapped by any shell, so a
# `kill -9` on this process leaks the cluster on its loopback port with a
# trust-auth connection until something notices and removes $PGDATA_DIR by
# hand. Short-lived local/CI helper, so that's an accepted cost.
trap cleanup EXIT INT TERM

echo "with-postgres: starting throwaway cluster on port ${PGPORT}" >&2
run_pg "'${PGBIN}/initdb' -D '${PGDATA_DIR}/data' -U cicado --auth=trust --no-sync -E UTF8" >/dev/null

# fsync off and a tiny WAL: this cluster exists for the length of one test run
# and is deleted afterwards, so durability buys nothing and costs wall time.
run_pg "'${PGBIN}/pg_ctl' -D '${PGDATA_DIR}/data' -l '${LOGFILE}' -w -t 60 -o \"-p ${PGPORT} -k '${PGDATA_DIR}' -c listen_addresses=127.0.0.1 -c fsync=off -c full_page_writes=off -c synchronous_commit=off\" start" >/dev/null

run_pg "'${PGBIN}/createdb' -h 127.0.0.1 -p ${PGPORT} -U cicado cicado_test"

export CICADO_TEST_DATABASE_URL="postgres://cicado@127.0.0.1:${PGPORT}/cicado_test?sslmode=disable"
echo "with-postgres: ready at ${CICADO_TEST_DATABASE_URL}" >&2

"$@"
