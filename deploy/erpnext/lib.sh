# Shared helpers for the ERPNext scripts in deploy/erpnext (CC-102).
# Source it; don't run it.

# The scripts handle passwords and API secrets; xtrace would print them.
case "$-" in *x*)
	echo "refusing to run with xtrace" >&2
	exit 1
	;;
esac

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
compose_file="$root/deploy/erpnext/docker-compose.yaml"
site="erp.localhost"

# Local-only development defaults. Override them in your shell; they are not in
# .env.example. ERP_DB_ROOT_PASSWORD must match MYSQL_ROOT_PASSWORD in
# docker-compose.yaml (frappe_docker's default, 123).
export ERP_ADMIN_PASSWORD="${ERP_ADMIN_PASSWORD:-admin}"
export ERP_DB_ROOT_PASSWORD="${ERP_DB_ROOT_PASSWORD:-123}"

dc() {
	docker compose -f "$compose_file" "$@"
}

# bench runs a bench command for the site inside the backend container.
bench() {
	dc exec -T backend bench --site "$site" "$@"
}

log() {
	echo "$(basename "$0"): $*"
}

die() {
	echo "$(basename "$0"): $*" >&2
	exit 1
}

# require_backend fails unless the backend container runs and the configurator
# has written the database and Redis hosts into common_site_config.json.
require_backend() {
	dc ps --status running --services 2>/dev/null | grep -qx backend ||
		die "the ERPNext backend is not running; run: make up"

	local deadline=$((SECONDS + 180))
	until dc exec -T backend jq -e '.db_host and .redis_cache and .redis_queue' \
		sites/common_site_config.json >/dev/null 2>&1; do
		((SECONDS < deadline)) ||
			die "sites/common_site_config.json has no db_host/redis settings; check: docker compose -f deploy/erpnext/docker-compose.yaml logs configurator"
		sleep 3
	done
}

# site_exists is true when the site's directory and config exist.
site_exists() {
	dc exec -T backend test -f "sites/$site/site_config.json"
}
