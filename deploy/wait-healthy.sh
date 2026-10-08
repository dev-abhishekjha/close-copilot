#!/usr/bin/env bash
# Waits until the local stack answers (CC-102): ERPNext's /login on 8080, TEI
# /health on 8081 (embeddings) and 8082 (reranker), and Postgres pg_isready.
#
# Usage: deploy/wait-healthy.sh [timeout-seconds]     (default 900)
#
# Exits 0 when every service is up. On timeout it exits 1 and names each
# service that is still down. On first start TEI downloads its models, which
# takes minutes.
set -euo pipefail

timeout="${1:-900}"
[[ "$timeout" =~ ^[0-9]+$ ]] || { echo "usage: $0 [timeout-seconds]" >&2; exit 2; }

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
erp_url="${ERP_BASE_URL:-http://localhost:8080}"
embed_url="${TEI_EMBED_URL:-http://localhost:8081}"
rerank_url="${TEI_RERANK_URL:-http://localhost:8082}"

http_ok() {
	curl -fsS -o /dev/null --max-time 5 "$1" 2>/dev/null
}

check() {
	case "$1" in
	erpnext) http_ok "$erp_url/login" ;;
	tei-embed) http_ok "$embed_url/health" ;;
	tei-rerank) http_ok "$rerank_url/health" ;;
	postgres)
		docker compose -f "$root/deploy/docker-compose.yml" exec -T postgres \
			pg_isready -q -U copilot -d copilot >/dev/null 2>&1
		;;
	esac
}

pending=(erpnext tei-embed tei-rerank postgres)
deadline=$((SECONDS + timeout))

while :; do
	still=()
	for svc in "${pending[@]}"; do
		if check "$svc"; then
			echo "wait-healthy: $svc is up"
		else
			still+=("$svc")
		fi
	done
	if ((${#still[@]} == 0)); then
		echo "wait-healthy: all services are up"
		exit 0
	fi
	pending=("${still[@]}")
	if ((SECONDS >= deadline)); then
		echo "wait-healthy: timed out after ${timeout}s; not up: ${pending[*]}" >&2
		exit 1
	fi
	sleep 5
done
