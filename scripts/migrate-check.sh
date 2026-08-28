#!/usr/bin/env bash
# Round-trip every migration on a throwaway database: up, down, up again.
#
# The upgrade procedure in deploy/README.md rests on two claims, and this is
# what makes them checkable rather than asserted:
#
#   1. The chain applies cleanly from an empty database. Every `make nuke`
#      proves that already, so it is the cheap half.
#   2. Every `down` actually undoes its `up`. Nothing else exercises the down
#      migrations at all — they are written once, never run, and discovered to
#      be wrong at the exact moment somebody needs to roll back. A down that
#      drops the wrong column or forgets an index is invisible until then.
#
# It also asserts that `up` on a current database is a no-op, because the
# documented upgrade runs it whether or not a release contains migrations, and
# an operator should never have to know which.
#
# Runs against its own database so it cannot disturb the dev stack or a test
# run sharing the same Postgres.
set -euo pipefail
cd "$(dirname "$0")/.."

# Reached over the published port with `docker run --network host` rather than
# through `docker compose exec`, so the same commands work against the dev
# stack's Postgres and against a CI service container. Nothing here needs the
# compose project to exist.
PGHOST=${PGHOST:-127.0.0.1}
PGPORT=${PGPORT:-5473}
PGUSER=${PGUSER:-composectl}
PGPASSWORD=${PGPASSWORD:-composectl}
DB="migrate_check_$$"

step() { printf '\n\033[36m▸ %s\033[0m\n' "$1"; }
note() { printf '  \033[90m%s\033[0m\n' "$1"; }
fail() {
    printf '\033[31mFAIL: %s\033[0m\n' "$1" >&2
    exit 1
}

pg() {
    local db=$1 sql=$2
    docker run --rm --network host -e PGPASSWORD="$PGPASSWORD" postgres:16-alpine \
        psql -h "$PGHOST" -p "$PGPORT" -U "$PGUSER" -d "$db" -tAc "$sql"
}
migrate() {
    docker run --rm --network host -v "$PWD/migrations:/migrations:ro" migrate/migrate:v4.18.1 \
        -path=/migrations \
        -database "postgres://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/$DB?sslmode=disable" "$@"
}

pg postgres 'SELECT 1' >/dev/null 2>&1 ||
    fail "no Postgres at $PGHOST:$PGPORT — run 'make up' first"

cleanup() { pg postgres "DROP DATABASE IF EXISTS $DB;" >/dev/null 2>&1 || true; }
trap cleanup EXIT

step "Every up migration has a down"
missing=0
for up in migrations/*.up.sql; do
    down=${up%.up.sql}.down.sql
    if [ ! -f "$down" ]; then
        printf '  \033[31mno down for %s\033[0m\n' "$(basename "$up")"
        missing=$((missing + 1))
    fi
done
[ "$missing" -eq 0 ] || fail "$missing migration(s) cannot be rolled back"
note "$(ls migrations/*.up.sql | wc -l | tr -d ' ') migrations, all reversible"

step "Creating $DB"
pg postgres "CREATE DATABASE $DB;" >/dev/null

step "up: empty → current"
migrate up >/dev/null
v1=$(pg "$DB" "SELECT version FROM schema_migrations;" | tr -d ' ')
note "at version $v1"

step "up again: must be a clean no-op"
# `migrate up` exits 0 and prints "no change" when there is nothing to apply.
# The documented upgrade runs this on every release, most of which add no
# migrations at all.
out=$(migrate up 2>&1 || fail "up on a current database returned non-zero")
case "$out" in
*"no change"*) note "no change, exit 0" ;;
*) fail "expected 'no change', got: $out" ;;
esac

step "down: current → empty"
migrate down -all >/dev/null
left=$(pg "$DB" "SELECT count(*) FROM information_schema.tables
     WHERE table_schema='public' AND table_name <> 'schema_migrations';" | tr -d ' ')
[ "$left" = "0" ] || fail "down -all left $left table(s) behind"
note "schema empty"

step "up: empty → current, again"
# The half that matters. If a down migration is wrong, this is where it shows:
# a column it failed to drop collides, or an index it left behind conflicts.
migrate up >/dev/null
v2=$(pg "$DB" "SELECT version FROM schema_migrations;" | tr -d ' ')
[ "$v2" = "$v1" ] || fail "re-applied chain landed at $v2, expected $v1"
note "back at version $v2"

printf '\n\033[32mMigrations round-trip cleanly — the documented upgrade and its rollback both hold.\033[0m\n'
