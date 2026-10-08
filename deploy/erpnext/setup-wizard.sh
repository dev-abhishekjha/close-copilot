#!/usr/bin/env bash
# Completes ERPNext's setup wizard on erp.localhost without demo data (CC-102):
# country India, currency INR, time zone Asia/Kolkata, the India chart of
# accounts, fiscal year April to March, first company
# "Sharma Traders Pvt Ltd" (abbreviation STPL).
#
# It then turns on Accounts Settings delete_linked_ledger_entries ("Delete
# Accounting and Stock Ledger Entries on deletion of Transaction") so that a
# cancelled voucher can be deleted together with its GL and stock ledger
# entries (AccountsController.on_trash). CC-202's integration test and CC-307's
# reset rely on it. This step runs on every invocation, also when the wizard
# was already complete.
#
# Usage: deploy/erpnext/setup-wizard.sh    (after new-site.sh)
#
# Idempotent: if the wizard is complete, the company exists and the setting is
# already 1, it exits 0 and changes nothing.
#
# It calls the same server method the browser wizard calls on version-15,
# frappe.desk.page.setup_wizard.setup_wizard.setup_complete, which runs the
# frappe, erpnext and india_compliance wizard stages. No first user is
# created; log in as Administrator. The audit trail stays off (it can't be
# turned off once on) and no company GSTIN is set: master data is CC-303's.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

company="Sharma Traders Pvt Ltd"
abbr="STPL"

require_backend
site_exists || die "site $site does not exist; run deploy/erpnext/new-site.sh first"

company_exists() {
	bench execute frappe.db.exists --args "[\"Company\", \"$company\"]" | grep -q "$company"
}

setup_complete() {
	bench execute frappe.is_setup_complete | grep -qx true
}

ledger_setting="delete_linked_ledger_entries"

# ledger_setting_value prints the setting's current value. bench execute prints
# nothing when the method returns a falsy value, so 0 reads back as "".
ledger_setting_value() {
	bench execute frappe.db.get_single_value --args "[\"Accounts Settings\", \"$ledger_setting\"]" |
		tail -n 1 | tr -d '[:space:]'
}

# enable_ledger_deletion sets Accounts Settings.delete_linked_ledger_entries to
# 1 unless it already is, then reads it back. bench execute commits after the
# call.
enable_ledger_deletion() {
	if [[ "$(ledger_setting_value)" == "1" ]]; then
		log "Accounts Settings $ledger_setting is already 1; no change"
		return
	fi
	log "setting Accounts Settings $ledger_setting = 1"
	bench execute frappe.db.set_single_value --args "[\"Accounts Settings\", \"$ledger_setting\", 1]"
	local got
	got="$(ledger_setting_value)"
	[[ "$got" == "1" ]] || die "Accounts Settings $ledger_setting reads back as '$got', want 1"
	log "ok: Accounts Settings $ledger_setting = 1"
}

run_wizard() {
	local year month fy_start fy_end kwargs details fy

	# The current Indian fiscal year: April 1 to March 31.
	year="$(date +%Y)"
	month="$((10#$(date +%m)))"
	((month >= 4)) || year=$((year - 1))
	fy_start="$year-04-01"
	fy_end="$((year + 1))-03-31"

	# bench execute eval()s --kwargs as a Python literal, so this JSON holds only
	# strings and integers (no true/false/null).
	kwargs="$(
		cat <<EOF
{"args": {
  "language": "English",
  "country": "India",
  "timezone": "Asia/Kolkata",
  "currency": "INR",
  "company_name": "$company",
  "company_abbr": "$abbr",
  "chart_of_accounts": "India - Chart of Accounts",
  "fy_start_date": "$fy_start",
  "fy_end_date": "$fy_end",
  "setup_demo": 0,
  "enable_telemetry": 0,
  "default_gst_rate": "18.0",
  "enable_audit_trail": 0
}}
EOF
	)"

	log "running the setup wizard: $company ($abbr), India, INR, fiscal year $fy_start to $fy_end"
	bench execute frappe.desk.page.setup_wizard.setup_wizard.setup_complete --kwargs "$kwargs"

	setup_complete || die "the setup wizard did not complete; see: docker compose -f deploy/erpnext/docker-compose.yaml logs backend"
	company_exists || die "the setup wizard completed but $company was not created"

	details="$(bench execute frappe.db.get_value --args "[\"Company\", \"$company\", [\"abbr\", \"country\", \"default_currency\"]]")"
	[[ "$details" == "[\"$abbr\", \"India\", \"INR\"]" ]] ||
		die "$company has unexpected settings: $details (want abbr $abbr, India, INR)"

	fy="$(bench execute frappe.db.get_value --args "[\"Fiscal Year\", {\"year_start_date\": \"$fy_start\", \"year_end_date\": \"$fy_end\"}, \"name\"]" | tr -d '"')"
	[[ -n "$fy" ]] || die "no fiscal year from $fy_start to $fy_end"

	log "ok: $company ($abbr), India, INR, fiscal year $fy ($fy_start to $fy_end)"
}

if setup_complete; then
	company_exists ||
		die "the setup wizard is complete but $company does not exist; reset the ERPNext volumes (docs/setup.md) and start again"
	log "setup wizard already complete and $company exists; no change"
else
	run_wizard
fi

# Runs on every invocation, also when the wizard was already complete.
enable_ledger_deletion
