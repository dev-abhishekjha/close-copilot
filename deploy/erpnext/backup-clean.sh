#!/usr/bin/env bash
# Takes a database backup of erp.localhost in its clean, bootstrapped state
# (CC-303) and copies it to a stable name that CC-307's reset restores:
#
#   sites/erp.localhost/private/backups/clean-bootstrap-database.sql.gz
#
# (relative to the bench directory inside the backend container). Run it
# after `go run ./cmd/seed bootstrap` and the bootstrap integration test,
# while holding tmp/erpnext.lock, so the backup has the masters and no
# business documents.
#
# Usage: deploy/erpnext/backup-clean.sh
#
# Idempotent: every run takes a fresh backup and overwrites the stable copy.
# It prints the stable path on stdout; bench's own output goes to stderr.
#
# Caveat for CC-307: Frappe prunes private/backups. Every `bench backup`
# first deletes files there whose ctime is over keep_backups_for_hours old
# (default 23 h), and a daily scheduler job keeps only backup_limit backup
# sets. Restore from this file soon after taking it, or re-run this script.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

backups="sites/$site/private/backups"
stable="$backups/clean-bootstrap-database.sql.gz"

require_backend
site_exists </dev/null || die "site $site does not exist; run deploy/erpnext/new-site.sh first"

log "backing up $site" >&2
bench backup </dev/null >&2

# The newest timestamped dump (bench names them
# <YYYYMMDD_HHMMSS>-<site_with_underscores>-database.sql.gz), never the
# stable copy itself.
newest="$(dc exec -T backend sh -c \
	"ls -1t $backups/*-database.sql.gz 2>/dev/null | grep -v '/clean-bootstrap-database.sql.gz\$' | head -n 1" </dev/null)"
newest="${newest//$'\r'/}"
[[ -n "$newest" ]] || die "bench backup wrote no database dump in $backups"

dc exec -T backend cp -f "$newest" "$stable" </dev/null
dc exec -T backend test -s "$stable" </dev/null || die "copying $newest to $stable failed"
log "copied $newest" >&2
echo "$stable"
