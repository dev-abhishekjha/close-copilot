#!/usr/bin/env bash
# Adds a company to erp.localhost (CC-008): country India, currency INR, the
# India chart of accounts, the fiscal year the setup wizard created. It sets
# no GSTIN; the seeder (cmd/seed bootstrap) sets the GST fields.
#
# Usage: deploy/erpnext/add-company.sh <company name> <abbr>
#   deploy/erpnext/add-company.sh "Mehta Tech Services Pvt Ltd" MTSPL
#
# Idempotent: if the company exists with that abbreviation it logs "no change"
# and exits 0. It fails if the abbreviation belongs to another company or the
# company exists with a different abbreviation.
#
# After adding a company, re-run deploy/erpnext/backup-clean.sh (under
# tmp/erpnext.lock) so `make erp-reset` restores the database with every
# company. Without that the reset drops the company again.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

(($# == 2)) || die "usage: add-company.sh <company name> <abbr>"
company="$1"
abbr="$2"
[[ "$company" =~ ^[A-Za-z0-9\ .\&-]+$ ]] || die "company name has characters outside letters, digits, space, . & -"
[[ "$abbr" =~ ^[A-Za-z0-9]{1,10}$ ]] || die "abbreviation must be 1 to 10 letters or digits"

require_backend
site_exists </dev/null || die "site $site does not exist; run deploy/erpnext/new-site.sh first"

# get_value prints the JSON list of fields for a Company, or nothing if absent.
get_value() {
	bench execute frappe.db.get_value --args "[\"Company\", $1, $2]" | tail -n 1
}

existing_abbr="$(get_value "\"$company\"" '"abbr"' | tr -d '"[:space:]')"
if [[ -n "$existing_abbr" && "$existing_abbr" != "None" ]]; then
	[[ "$existing_abbr" == "$abbr" ]] ||
		die "$company already exists with abbreviation $existing_abbr, not $abbr"
	log "$company ($abbr) already exists; no change"
	exit 0
fi

owner="$(get_value "{\"abbr\": \"$abbr\"}" '"name"' | tr -d '"')"
if [[ -n "$owner" && "$owner" != "None" ]]; then
	die "abbreviation $abbr belongs to $owner"
fi

# The company inherits the first company's fiscal year; read the current one.
year="$(date +%Y)"
month="$((10#$(date +%m)))"
((month >= 4)) || year=$((year - 1))
fy_start="$year-04-01"
fy_end="$((year + 1))-03-31"
fy="$(bench execute frappe.db.get_value --args "[\"Fiscal Year\", {\"year_start_date\": \"$fy_start\", \"year_end_date\": \"$fy_end\"}, \"name\"]" | tail -n 1 | tr -d '"')"
[[ -n "$fy" && "$fy" != "None" ]] || die "no fiscal year from $fy_start to $fy_end; run setup-wizard.sh first"

log "creating $company ($abbr), India, INR, chart India - Chart of Accounts"
bench execute frappe.client.insert --kwargs "{\"doc\": {
  \"doctype\": \"Company\",
  \"company_name\": \"$company\",
  \"abbr\": \"$abbr\",
  \"country\": \"India\",
  \"default_currency\": \"INR\",
  \"create_chart_of_accounts_based_on\": \"Standard Template\",
  \"chart_of_accounts\": \"India - Chart of Accounts\"
}}" >/dev/null

details="$(get_value "\"$company\"" '["abbr", "country", "default_currency"]')"
[[ "$details" == "[\"$abbr\", \"India\", \"INR\"]" ]] ||
	die "$company has unexpected settings: $details (want abbr $abbr, India, INR)"

log "ok: $company ($abbr), India, INR, fiscal year $fy ($fy_start to $fy_end)"
