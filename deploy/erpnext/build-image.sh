#!/usr/bin/env bash
# Builds close-copilot/erpnext:15: Frappe, ERPNext and India Compliance on
# version-15, from frappe_docker's images/custom/Containerfile (CC-102).
#
# Usage: deploy/erpnext/build-image.sh            (takes 15-30 minutes)
#
# The pinned frappe_docker reads apps.json as a BuildKit secret
# (--secret id=apps_json), not as the older APPS_JSON_BASE64 build arg; a
# build arg would be ignored and the image would hold only Frappe. See
# docs/setup.md.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fd="$root/deploy/frappe_docker"
apps_json="$root/deploy/erpnext/apps.json"
image="close-copilot/erpnext:15"

if [[ ! -f "$fd/images/custom/Containerfile" ]]; then
	echo "build-image: $fd is empty; run: git submodule update --init deploy/frappe_docker" >&2
	exit 1
fi

# Fail early on malformed JSON rather than 20 minutes into the build.
if command -v python3 >/dev/null 2>&1; then
	python3 -c 'import json, sys; json.load(open(sys.argv[1]))' "$apps_json" ||
		{ echo "build-image: $apps_json is not valid JSON" >&2; exit 1; }
fi

# Version-15 toolchain, as frappe_docker's own v15 CI build uses it
# (.github/workflows/core-build-stable.yml): Python 3.11, Node 22, bookworm.
echo "build-image: building $image (native platform: $(docker version --format '{{.Server.Arch}}'))"
docker build \
	--build-arg=FRAPPE_PATH=https://github.com/frappe/frappe \
	--build-arg=FRAPPE_BRANCH=version-15 \
	--build-arg=PYTHON_VERSION=3.11 \
	--build-arg=NODE_VERSION=22 \
	--build-arg=DEBIAN_BASE=bookworm \
	--secret=id=apps_json,src="$apps_json" \
	--tag="$image" \
	--file="$fd/images/custom/Containerfile" \
	"$fd"

# The image must match the engine's architecture (arm64 on Apple Silicon);
# an amd64 image would run under emulation and be very slow.
engine_arch="$(docker version --format '{{.Server.Arch}}')"
image_arch="$(docker image inspect --format '{{.Architecture}}' "$image")"
if [[ "$image_arch" != "$engine_arch" ]]; then
	echo "build-image: $image is $image_arch but the Docker engine is $engine_arch" >&2
	exit 1
fi

apps="$(docker run --rm --entrypoint ls "$image" /home/frappe/frappe-bench/apps)"
for app in frappe erpnext india_compliance; do
	if ! grep -qx "$app" <<<"$apps"; then
		echo "build-image: $image is missing the $app app (apps: $(tr '\n' ' ' <<<"$apps"))" >&2
		exit 1
	fi
done

echo "build-image: ok: $image ($image_arch) with apps: $(tr '\n' ' ' <<<"$apps")"
