#!/usr/bin/env bash
# This script handles API secrets; xtrace would print them.
case "$-" in *x*)
	echo "refusing to run with xtrace" >&2
	exit 1
	;;
esac

# Creates the ERPNext API credentials for erp.localhost (CC-201, CC-1007):
#
#   - user copilot-bot@example.com (enabled, System User, role Accounts User
#     only) for the read tools of the Books MCP server (/mcp), and its API
#     key pair (ERP_API_KEY, ERP_API_SECRET);
#   - user copilot-poster@example.com (enabled, System User, only the custom
#     role Copilot Poster, no User Permissions) for the one write tool,
#     post_approved_journal_entry on /mcp-admin, and its API key pair
#     (ERP_POST_API_KEY, ERP_POST_API_SECRET);
#   - an API key pair for Administrator, for the seeder (local development
#     only; ERP_SEED_API_KEY, ERP_SEED_API_SECRET).
#
# The role Copilot Poster may do exactly this, and nothing else:
#   Journal Entry, permlevel 0: read, write, create, submit
#     (no select, delete, cancel, amend, report, export, import, share,
#     print or email)
#   Extra read DocPerms on master DocTypes: none
# The rules are granted with frappe.permissions.add_permission and
# update_permission_property, which first copy a DocType's standard rules
# into Custom DocPerm (once any Custom DocPerm exists for a DocType, Frappe
# ignores its standard DocPerm rows). A Custom DocPerm document is never
# inserted directly: that would strip Journal Entry rights from Accounts User,
# Accounts Manager and every other role. After the grant the script checks
# that Accounts User and Accounts Manager still read and write Journal
# Entries. A rule of the role that differs from the list above stops the
# script; fix it in the Role Permission Manager and run again.
#
# All six key values, plus ERP_BASE_URL and ERP_SITE, go to tmp/erp-keys.env
# (git-ignored, mode 600). Run a command that needs them in a subshell, so
# the keys are never exported into your own shell:
#   (set -a; . tmp/erp-keys.env; set +a; <command>)
# The seeder's ERP_SEED_* pair lives only in that file, never in .env. The
# posting pair goes only into the environment of mcp-books (the /mcp-admin
# route). No secret is ever printed, logged or passed on a command line.
#
# ERP_BASE_URL (default http://localhost:8080) must be a bare
# http(s)://host[:port] URL; it is checked before anything is written.
#
# Usage: deploy/erpnext/api-users.sh [--rotate]
#
# Idempotent: when both users and the role exist as described and
# tmp/erp-keys.env already holds the three users' current key IDs, a second
# run changes nothing and leaves the file as it is. A user whose key is
# missing, or whose key ID doesn't match the file, gets a fresh pair (Frappe
# shows a secret only once, when it is generated). --rotate regenerates all
# three pairs, key IDs included, and rewrites the file.
#
# It calls, through bench inside the backend container:
#   frappe.client.insert                            create the users and the role
#   frappe.permissions.add_permission               add the role's Journal Entry rule
#   frappe.permissions.update_permission_property   set each flag of that rule
#   frappe.permissions.get_all_perms                check the standard roles' rules
#   frappe.clear_cache(doctype=Journal Entry)       after a permission change
#   frappe.core.doctype.user.user.generate_keys     returns {api_key, api_secret}
#   frappe.db.set_value(User, <user>, api_key, None) before generate_keys, on --rotate
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

bot="copilot-bot@example.com"
bot_role="Accounts User"
poster="copilot-poster@example.com"
poster_role="Copilot Poster"
admin="Administrator"
keys_file="$root/tmp/erp-keys.env"
base_url="${ERP_BASE_URL:-http://localhost:8080}"

# The URL goes into the keys file, which is sourced as shell, so it must be
# a plain scheme, host and optional port: no path, quotes, spaces or $.
base_url_re='^https?://[A-Za-z0-9.-]+(:[0-9]+)?$'
[[ "$base_url" =~ $base_url_re ]] ||
	die "ERP_BASE_URL must look like http://host[:port] (letters, digits, dots and hyphens; no path)"

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

# 2. The posting role and its permissions.
je="Journal Entry"
# Every Check field of Custom DocPerm except if_owner, and the flags the role
# gets on Journal Entry. Every other flag must be 0.
perm_flags='["select", "read", "write", "create", "delete", "submit", "cancel", "amend", "report", "export", "import", "share", "print", "email"]'
poster_want="{\"$je\": [\"read\", \"write\", \"create\", \"submit\"]}"

# poster_rules prints the role's Custom DocPerm rows as a JSON array.
poster_rules() {
	local out
	out="$(bench execute frappe.get_all --kwargs "{\"doctype\": \"Custom DocPerm\", \"filters\": {\"role\": \"$poster_role\"}, \"fields\": \"*\", \"order_by\": \"parent asc, permlevel asc\"}")"
	printf '%s' "${out:-[]}"
}

if bench execute frappe.db.exists --args "[\"Role\", \"$poster_role\"]" | grep -q "$poster_role"; then
	log "role $poster_role exists"
else
	log "creating role $poster_role"
	bench execute frappe.client.insert --kwargs "{\"doc\": {\"doctype\": \"Role\", \"role_name\": \"$poster_role\", \"desk_access\": 1}}" >/dev/null
fi
details="$(bench execute frappe.db.get_value --args "[\"Role\", \"$poster_role\", [\"disabled\", \"desk_access\"]]")"
[[ "$details" == "[0, 1]" ]] ||
	die "role $poster_role has unexpected settings: $details (want [disabled, desk_access] = [0, 1]); fix it in the desk and run again"

# The rule is set flag by flag only when this run creates it; an existing rule
# that differs from the list in the header stops the script (below).
rules="$(poster_rules)"
if [[ "$(printf '%s' "$rules" | jq --arg dt "$je" '[.[] | select(.parent == $dt)] | length')" == 0 ]]; then
	log "granting $poster_role its $je rule"
	bench execute frappe.permissions.add_permission --args "[\"$je\", \"$poster_role\", 0]" >/dev/null
	for flag in $(jq -rn --argjson all "$perm_flags" '$all[]'); do
		value=0
		if jq -en --argjson want "$poster_want" --arg dt "$je" --arg f "$flag" '$want[$dt] | index($f)' >/dev/null; then
			value=1
		fi
		bench execute frappe.permissions.update_permission_property --args "[\"$je\", \"$poster_role\", 0, \"$flag\", $value]" >/dev/null
	done
	bench execute frappe.clear_cache --kwargs "{\"doctype\": \"$je\"}" >/dev/null
	rules="$(poster_rules)"
fi

# The role's rules must be exactly the wanted set: one permlevel 0 rule per
# listed DocType, not if_owner, with the listed flags 1 and every other 0.
drift="$(printf '%s' "$rules" | jq -r --argjson want "$poster_want" --argjson flags "$perm_flags" '
	. as $rows
	| ( $rows[]
		| . as $r
		| if ($want[$r.parent] == null) then "\($r.parent): unexpected rule (permlevel \($r.permlevel))"
		  elif ($r.permlevel != 0 or $r.if_owner != 0) then "\($r.parent): unexpected rule (permlevel \($r.permlevel), if_owner \($r.if_owner))"
		  else ($flags[] as $f
			| (if ($want[$r.parent] | index($f)) then 1 else 0 end) as $w
			| select(($r[$f] // 0) != $w)
			| "\($r.parent): \($f) is \($r[$f] // 0), want \($w)")
		  end ),
	  ( $want | keys[] as $d
		| ([$rows[] | select(.parent == $d and .permlevel == 0)] | length) as $n
		| select($n != 1)
		| "\($d): \($n) permlevel 0 rules, want 1" )')"
[[ -z "$drift" ]] ||
	die "role $poster_role has unexpected permissions: $(printf '%s' "$drift" | paste -sd ';' -); fix them in the Role Permission Manager and run again"

# The standard roles keep their Journal Entry rights (the custom rules
# replaced the standard ones for the whole DocType).
for role in "Accounts User" "Accounts Manager"; do
	ok="$(bench execute frappe.permissions.get_all_perms --args "[\"$role\"]" |
		jq --arg dt "$je" '[.[] | select(.parent == $dt and .permlevel == 0 and .read == 1 and .write == 1)] | length > 0')"
	[[ "$ok" == true ]] ||
		die "role $role lost read or write on $je; restore it in the Role Permission Manager (Reset to defaults) and run again"
done
log "role $poster_role may read, write, create and submit $je and nothing else; Accounts User and Accounts Manager keep theirs"

# 3. The posting user.
if user_exists "$poster"; then
	log "user $poster exists"
else
	log "creating user $poster with role $poster_role"
	doc="{\"doc\": {\"doctype\": \"User\", \"email\": \"$poster\", \"first_name\": \"Copilot\", \"last_name\": \"Poster\", \"user_type\": \"System User\", \"enabled\": 1, \"send_welcome_email\": 0, \"roles\": [{\"role\": \"$poster_role\"}]}}"
	bench execute frappe.client.insert --kwargs "$doc" >/dev/null
	user_exists "$poster" || die "user $poster was not created"
fi

details="$(bench execute frappe.db.get_value --args "[\"User\", \"$poster\", [\"enabled\", \"user_type\"]]")"
[[ "$details" == "[1, \"System User\"]" ]] ||
	die "user $poster has unexpected settings: $details (want enabled, System User); fix it in the desk or delete the user and run again"
roles="$(bench execute frappe.get_all --kwargs "{\"doctype\": \"Has Role\", \"filters\": {\"parent\": \"$poster\", \"parenttype\": \"User\"}, \"pluck\": \"role\", \"order_by\": \"role asc\"}")"
[[ "$roles" == "[\"$poster_role\"]" ]] ||
	die "user $poster has roles $roles, want only [\"$poster_role\"]; fix them in the desk and run again"
uperms="$(bench execute frappe.get_all --kwargs "{\"doctype\": \"User Permission\", \"filters\": {\"user\": \"$poster\"}, \"pluck\": \"name\"}")"
[[ -z "$uperms" || "$uperms" == "[]" ]] ||
	die "user $poster has User Permissions $uperms, want none; remove them in the desk and run again"

# 4. The key pairs. Each user keeps the pair in the file unless its key is
# missing, its key ID doesn't match the file, or --rotate was given.
bot_key="$(file_value ERP_API_KEY)"
bot_secret="$(file_value ERP_API_SECRET)"
seed_key="$(file_value ERP_SEED_API_KEY)"
seed_secret="$(file_value ERP_SEED_API_SECRET)"
post_key="$(file_value ERP_POST_API_KEY)"
post_secret="$(file_value ERP_POST_API_SECRET)"

current="$(api_key "$bot")"
if ((rotate)) || [[ -z "$current" || "$current" != "$bot_key" || -z "$bot_secret" ]]; then
	log "generating an API key pair for $bot"
	generate "$bot"
	bot_key="$new_key"
	bot_secret="$new_secret"
else
	log "$bot already has the key in $keys_file"
fi

current="$(api_key "$poster")"
if ((rotate)) || [[ -z "$current" || "$current" != "$post_key" || -z "$post_secret" ]]; then
	log "generating an API key pair for $poster"
	generate "$poster"
	post_key="$new_key"
	post_secret="$new_secret"
else
	log "$poster already has the key in $keys_file"
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

# 5. The keys file. printf is a builtin, so no secret shows up in a process
# listing. The file is rewritten only when its content changes.
mkdir -p "$root/tmp"
tmp_file="$(mktemp "$root/tmp/erp-keys.env.XXXXXX")"
trap 'rm -f "$tmp_file"' EXIT
{
	printf '# ERPNext API credentials for %s (deploy/erpnext/api-users.sh). Local development only.\n' "$site"
	printf '# Use in a subshell only: (set -a; . tmp/erp-keys.env; set +a; <command>)\n'
	printf '# ERP_SEED_* stay in this file, never in .env. ERP_POST_* go only to mcp-books (/mcp-admin).\n'
	printf '# Never commit or print these values.\n'
	printf 'ERP_BASE_URL=%s\n' "$base_url"
	printf 'ERP_SITE=%s\n' "$site"
	printf 'ERP_API_KEY=%s\n' "$bot_key"
	printf 'ERP_API_SECRET=%s\n' "$bot_secret"
	printf 'ERP_SEED_API_KEY=%s\n' "$seed_key"
	printf 'ERP_SEED_API_SECRET=%s\n' "$seed_secret"
	printf 'ERP_POST_API_KEY=%s\n' "$post_key"
	printf 'ERP_POST_API_SECRET=%s\n' "$post_secret"
} >"$tmp_file"
bot_secret=""
seed_secret=""
post_secret=""

if [[ -f "$keys_file" ]] && cmp -s "$tmp_file" "$keys_file"; then
	log "$keys_file is up to date; nothing changed"
else
	chmod 600 "$tmp_file"
	mv -f "$tmp_file" "$keys_file"
	log "wrote $keys_file (mode 600)"
fi
chmod 600 "$keys_file"

log "ok: $bot ($bot_role), $poster ($poster_role) and $admin have API keys"
log "next: (set -a; . tmp/erp-keys.env; set +a; go run ./cmd/probe auth)"
log "      the subshell keeps the keys out of your shell; the seeder keys stay in tmp/erp-keys.env, never in .env"
