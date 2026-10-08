#!/usr/bin/env bash
# Creates the ERPNext API credentials for erp.localhost (CC-201):
#
#   - user copilot-bot@example.com (enabled, System User, role Accounts User
#     only) for the Books MCP server, and its API key pair;
#   - an API key pair for Administrator, for the seeder (local development
#     only).
#
# All four values, plus ERP_BASE_URL and ERP_SITE, go to tmp/erp-keys.env
# (git-ignored, mode 600). Copy its lines into your .env. No secret is ever
# printed.
#
# Usage: deploy/erpnext/api-users.sh [--rotate]
#
# Idempotent: when the bot exists and tmp/erp-keys.env already holds both
# users' current key IDs, a second run changes nothing and leaves the file as
# it is. A user whose key is missing, or whose key ID doesn't match the file,
# gets a fresh pair (Frappe shows a secret only once, when it is generated).
# --rotate regenerates both pairs, key IDs included, and rewrites the file.
#
# It calls, through bench inside the backend container:
#   frappe.client.insert                            create the bot user
#   frappe.core.doctype.user.user.generate_keys     returns {api_key, api_secret}
#   frappe.db.set_value(User, <user>, api_key, None) before generate_keys, on --rotate
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

bot="copilot-bot@example.com"
bot_role="Accounts User"
admin="Administrator"
keys_file="$root/tmp/erp-keys.env"
base_url="${ERP_BASE_URL:-http://localhost:8080}"

rotate=0
case "${1:-}" in
"") ;;
--rotate) rotate=1 ;;
*) die "usage: $0 [--rotate]" ;;
esac

umask 077

command -v jq >/dev/null || die "jq is required (macOS 15+ ships /usr/bin/jq; otherwise: brew install jq)"
require_backend
site_exists || die "site $site does not exist; run deploy/erpnext/new-site.sh first"

# bench execute eval()s --args and --kwargs as Python literals, so the JSON
# below holds only strings, integers and None.

user_exists() {
	bench execute frappe.db.exists --args "[\"User\", \"$1\"]" | grep -q "$1"
}

# api_key prints the user's key ID, or nothing when it has none.
api_key() {
	bench execute frappe.db.get_value --args "[\"User\", \"$1\", \"api_key\"]" | tr -d '"[:space:]'
}

# file_value prints the value of KEY in the existing keys file, if any.
file_value() {
	[[ -f "$keys_file" ]] || return 0
	sed -n "s/^$1=//p" "$keys_file" | head -n 1
}

# generate sets the global variables new_key and new_secret for user $1.
generate() {
	local user="$1" out
	if ((rotate)); then
		bench execute frappe.db.set_value --args "[\"User\", \"$user\", \"api_key\", None]" >/dev/null
	fi
	out="$(bench execute frappe.core.doctype.user.user.generate_keys --args "[\"$user\"]")"
	new_key="$(printf '%s' "$out" | jq -r '.api_key // empty')"
	new_secret="$(printf '%s' "$out" | jq -r '.api_secret // empty')"
	out=""
	[[ -n "$new_key" && -n "$new_secret" ]] || die "generate_keys returned no key pair for $user"
	[[ "$(api_key "$user")" == "$new_key" ]] || die "the key ID generated for $user was not saved"
}

# 1. The bot user.
if user_exists "$bot"; then
	log "user $bot exists"
else
	log "creating user $bot with role $bot_role"
	doc="{\"doc\": {\"doctype\": \"User\", \"email\": \"$bot\", \"first_name\": \"Copilot\", \"last_name\": \"Bot\", \"user_type\": \"System User\", \"enabled\": 1, \"send_welcome_email\": 0, \"roles\": [{\"role\": \"$bot_role\"}]}}"
	bench execute frappe.client.insert --kwargs "$doc" >/dev/null
	user_exists "$bot" || die "user $bot was not created"
fi

details="$(bench execute frappe.db.get_value --args "[\"User\", \"$bot\", [\"enabled\", \"user_type\"]]")"
[[ "$details" == "[1, \"System User\"]" ]] ||
	die "user $bot has unexpected settings: $details (want enabled, System User); fix it in the desk or delete the user and run again"
roles="$(bench execute frappe.get_all --kwargs "{\"doctype\": \"Has Role\", \"filters\": {\"parent\": \"$bot\", \"parenttype\": \"User\"}, \"pluck\": \"role\", \"order_by\": \"role asc\"}")"
[[ "$roles" == "[\"$bot_role\"]" ]] ||
	die "user $bot has roles $roles, want only [\"$bot_role\"]; fix them in the desk and run again"

# 2. The key pairs. Each user keeps the pair in the file unless its key is
# missing, its key ID doesn't match the file, or --rotate was given.
bot_key="$(file_value ERP_API_KEY)"
bot_secret="$(file_value ERP_API_SECRET)"
seed_key="$(file_value ERP_SEED_API_KEY)"
seed_secret="$(file_value ERP_SEED_API_SECRET)"

current="$(api_key "$bot")"
if ((rotate)) || [[ -z "$current" || "$current" != "$bot_key" || -z "$bot_secret" ]]; then
	log "generating an API key pair for $bot"
	generate "$bot"
	bot_key="$new_key"
	bot_secret="$new_secret"
else
	log "$bot already has the key in $keys_file"
fi

current="$(api_key "$admin")"
if ((rotate)) || [[ -z "$current" || "$current" != "$seed_key" || -z "$seed_secret" ]]; then
	log "generating an API key pair for $admin (seeder, local development only)"
	generate "$admin"
	seed_key="$new_key"
	seed_secret="$new_secret"
else
	log "$admin already has the key in $keys_file"
fi
new_key=""
new_secret=""

# 3. The keys file. printf is a builtin, so no secret shows up in a process
# listing. The file is rewritten only when its content changes.
mkdir -p "$root/tmp"
tmp_file="$(mktemp "$root/tmp/erp-keys.env.XXXXXX")"
trap 'rm -f "$tmp_file"' EXIT
{
	printf '# ERPNext API credentials for %s (deploy/erpnext/api-users.sh). Local development only.\n' "$site"
	printf '# Copy these lines into .env. Never commit or print them.\n'
	printf 'ERP_BASE_URL=%s\n' "$base_url"
	printf 'ERP_SITE=%s\n' "$site"
	printf 'ERP_API_KEY=%s\n' "$bot_key"
	printf 'ERP_API_SECRET=%s\n' "$bot_secret"
	printf 'ERP_SEED_API_KEY=%s\n' "$seed_key"
	printf 'ERP_SEED_API_SECRET=%s\n' "$seed_secret"
} >"$tmp_file"
bot_secret=""
seed_secret=""

if [[ -f "$keys_file" ]] && cmp -s "$tmp_file" "$keys_file"; then
	log "$keys_file is up to date; nothing changed"
else
	chmod 600 "$tmp_file"
	mv -f "$tmp_file" "$keys_file"
	log "wrote $keys_file (mode 600)"
fi
chmod 600 "$keys_file"

log "ok: $bot ($bot_role) and $admin have API keys"
log "next: copy the ERP_* lines from tmp/erp-keys.env into .env (for example: grep '^ERP_' tmp/erp-keys.env >> .env, after removing the empty ERP_* lines there), then run: go run ./cmd/probe auth"
