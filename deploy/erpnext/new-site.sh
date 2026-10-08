#!/usr/bin/env bash
# Creates the site erp.localhost with ERPNext and India Compliance (CC-102).
#
# Usage: deploy/erpnext/new-site.sh        (after `make up`; takes a few minutes)
#
# Idempotent: if the site already exists with both apps, it exits 0 and
# changes nothing. Reads ERP_ADMIN_PASSWORD (default admin) and
# ERP_DB_ROOT_PASSWORD (default 123) from the environment; both defaults are
# for local development only.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

require_backend

if site_exists; then
	apps="$(bench list-apps)"
	if grep -q '^erpnext' <<<"$apps" && grep -q '^india_compliance' <<<"$apps"; then
		log "site $site already exists with erpnext and india_compliance; nothing to do"
		exit 0
	fi
	die "site $site exists but lacks erpnext or india_compliance (apps: $(tr '\n' ' ' <<<"$apps")); reset the ERPNext volumes (docs/setup.md) and run this again"
fi

log "creating site $site with erpnext and india_compliance"
# The passwords travel as environment variables into the container, so they
# never appear in this host's process list or in the log.
dc exec -T -e ERP_ADMIN_PASSWORD -e ERP_DB_ROOT_PASSWORD backend bash -c '
	bench new-site "'"$site"'" \
		--mariadb-user-host-login-scope=% \
		--db-root-password "$ERP_DB_ROOT_PASSWORD" \
		--admin-password "$ERP_ADMIN_PASSWORD" \
		--install-app erpnext \
		--install-app india_compliance \
		--set-default
'

apps="$(bench list-apps)"
grep -q '^erpnext' <<<"$apps" || die "erpnext is not installed on $site"
grep -q '^india_compliance' <<<"$apps" || die "india_compliance is not installed on $site"
log "ok: $site with $(grep -v '^$' <<<"$apps" | awk '{print $1}' | tr '\n' ' ')"
