# Local setup

How to get from a clean machine to a running stack: ERPNext with India Compliance, Postgres with pgvector, and the two TEI model servers (CC-102). Run every command from the repository root.

## What runs where

| Service | Compose file | Port | Notes |
| --- | --- | --- | --- |
| ERPNext `frontend` (nginx) | `deploy/erpnext/docker-compose.yaml` | 8080 | Serves the site `erp.localhost` for any host name |
| ERPNext `backend`, `websocket`, `queue-short`, `queue-long`, `scheduler`, `db` (MariaDB 11.8), `redis-cache`, `redis-queue` | same | none | Compose project `close-copilot-erpnext` |
| `postgres` (pgvector, pg17) | `deploy/docker-compose.yml` | 5432 | Compose project `close-copilot` |
| `tei-embed` (`BAAI/bge-small-en-v1.5`) | same | 8081 | |
| `tei-rerank` (`BAAI/bge-reranker-base`) | same | 8082 | |
| `docling` | same, profile `ingest` | 5001 | Only with `make up-ingest` |

`make up` and `make down` drive both compose files.

## 1. Prerequisites

- **Docker Desktop** (Docker Engine 23 or later, with BuildKit and Compose v2.24 or later). Under *Settings → Resources*, give it about **10 GB of memory**. ERPNext's eight containers take about 3 GB, the TEI servers and Postgres about 1.5 GB, and the image build needs room on top. With less (8 GB works), stop other containers while you build.
- About 15 GB of free disk for images and volumes.
- `git`, `make`, `curl`. Go 1.26+ for the rest of the repo.
- Network access to GitHub, Docker Hub, ghcr.io and Hugging Face for the first build and start.

## 2. Clone with the submodule

frappe_docker is a git submodule at `deploy/frappe_docker`:

```sh
git clone --recurse-submodules <repo-url> close_copilot
# or, in an existing checkout:
git submodule update --init deploy/frappe_docker
```

### Pinned frappe_docker commit

| | |
| --- | --- |
| Commit | `e08b50ca057139c6e8c9117838ee3f5c5ee64a37` (main, 2026-10-06, "chore: Update example.env") |
| Pinned on | 2026-10-08 (CC-102) |

Why this commit:

- It was the tip of `main` when CC-102 was built, and frappe_docker's own CI builds and tests version-15 images from it (`.github/workflows/core-build-stable.yml`, job `v15_test`, with Python 3.11, Node 22 and Debian bookworm). `build-image.sh` uses the same toolchain.
- Its `images/custom/Containerfile` takes `apps.json` as a BuildKit secret (`--secret id=apps_json`). Older commits took it as the build arg `APPS_JSON_BASE64`, which leaves the file in the image history. This commit ignores that build arg, so the older command in the ticket would build an image with Frappe only.

To move the pin, check out another commit in `deploy/frappe_docker`, run `deploy/erpnext/generate-compose.sh` and `deploy/erpnext/build-image.sh`, go through steps 4 to 7 on fresh volumes, and update this table.

## 3. Environment file

```sh
cp .env.example .env
```

On Apple Silicon, set `TEI_IMAGE_TAG=cpu-arm64-1.9` in `.env` (see *Apple Silicon* below). The other values aren't needed to start the stack. The ERPNext API keys come later, in CC-201.

The ERPNext scripts read two passwords from the environment. They are **not** in `.env.example`, and their defaults are for local development only:

| Variable | Default | Used by |
| --- | --- | --- |
| `ERP_ADMIN_PASSWORD` | `admin` | `new-site.sh`: password of ERPNext's `Administrator` user |
| `ERP_DB_ROOT_PASSWORD` | `123` | `new-site.sh`: MariaDB root password. It must match `MYSQL_ROOT_PASSWORD` in `deploy/erpnext/docker-compose.yaml`, which is frappe_docker's development default `123` |

Set them in your shell before step 6 if you want other values (for example `export ERP_ADMIN_PASSWORD=...`). The admin password only takes effect when the site is created.

## 4. Build the ERPNext image (15 to 30 minutes, once)

```sh
deploy/erpnext/build-image.sh
```

This builds `close-copilot/erpnext:15` from `deploy/frappe_docker/images/custom/Containerfile` with:

- `FRAPPE_BRANCH=version-15`, plus `PYTHON_VERSION=3.11`, `NODE_VERSION=22` and `DEBIAN_BASE=bookworm` (the version-15 toolchain; the Containerfile's defaults target version-16);
- `deploy/erpnext/apps.json` (ERPNext and India Compliance, both on `version-15`) passed as the BuildKit secret `apps_json`.

When the build finishes, the script checks that the image's architecture matches the Docker engine's (`arm64` on Apple Silicon) and that the image holds the `frappe`, `erpnext` and `india_compliance` apps. Rebuild after changing `apps.json` or the submodule pin. The branches move, so a rebuild picks up their latest commits.

## 5. Start the stack

```sh
make up
```

On the first start:

- TEI downloads its two models into the `tei-models` volume, which takes a few minutes.
- ERPNext's `configurator` container writes the database and Redis hosts into `sites/common_site_config.json` and exits. That exit is expected.
- `http://localhost:8080` returns 404 until the site exists (step 6).

## 6. Create the site

```sh
deploy/erpnext/new-site.sh
```

This runs `bench new-site erp.localhost --mariadb-user-host-login-scope=% --db-root-password … --admin-password … --install-app erpnext --install-app india_compliance --set-default` inside the `backend` container. The scope flag lets the site's database user log in from the other containers. It takes a few minutes.

The script is idempotent. If `erp.localhost` already exists with both apps, it changes nothing and exits 0. If the site exists without both apps (a failed earlier run), it stops and asks you to reset the volumes (see *Reset ERPNext*).

The frontend has `FRAPPE_SITE_NAME_HEADER=erp.localhost`, so `http://localhost:8080` serves this site without a hosts-file entry. API clients send `Host: erp.localhost` anyway (`ERP_SITE` in `.env`).

## 7. Run the setup wizard

```sh
deploy/erpnext/setup-wizard.sh
```

This finishes ERPNext's setup wizard **without demo data**. It runs `bench execute frappe.desk.page.setup_wizard.setup_wizard.setup_complete`, the same server method the browser wizard calls on version-15, which runs the Frappe, ERPNext and India Compliance setup stages:

| Setting | Value |
| --- | --- |
| Language, country, time zone | English, India, Asia/Kolkata |
| Currency | INR |
| Company | Sharma Traders Pvt Ltd, abbreviation `STPL` |
| Chart of accounts | India - Chart of Accounts (the wizard's default for India) |
| Fiscal year | April 1 to March 31, the year that contains today |
| Demo data | none |
| India Compliance | default GST rate 18%, no company GSTIN, audit trail off |

The script then checks the company's abbreviation, country, currency and fiscal year. It is idempotent: if the wizard is complete and the company exists, it changes nothing.

It creates no first user. Log in as `Administrator` with `ERP_ADMIN_PASSWORD`. The audit trail is left off because India Compliance can't turn it off once it's on, and seeding and resets (CC-303, CC-307) may need to delete documents. Master data (GSTIN, addresses, suppliers, more fiscal years) is CC-303's job.

If the script fails, the browser wizard does the same thing. Open `http://localhost:8080`, log in as `Administrator`, and enter the values from the table: untick *Generate Demo Data*, leave *Enable Audit Trail* unticked and the GSTIN empty, and skip or fill the first-user slide.

## 8. Check it

With the stack running:

```sh
deploy/wait-healthy.sh 900
curl -fsS -o /dev/null http://localhost:8080/login
docker compose -f deploy/erpnext/docker-compose.yaml exec -T backend bench --site erp.localhost list-apps
curl -fsS http://localhost:8081/health
curl -fsS http://localhost:8082/health
docker compose -f deploy/docker-compose.yml exec -T postgres psql -U copilot -d copilot -c 'select 1'
```

`deploy/wait-healthy.sh <timeout-seconds>` polls ERPNext's `/login`, TEI's `/health` on 8081 and 8082, and Postgres `pg_isready`. It exits 0 when all are up. Otherwise it exits 1 and names the services that aren't up yet. Give it 900 seconds on a first start, while TEI downloads its models.

Then open `http://localhost:8080` and log in as `Administrator`.

## 9. Stop

```sh
make down
```

This stops and removes the containers. Volumes, and so the site, the database and the TEI models, stay. The next `make up` brings back the same ERPNext. You only need steps 6 and 7 again after a reset.

## Apple Silicon (arm64)

- **TEI**: the default tag `cpu-1.9` is x86-only. Set `TEI_IMAGE_TAG=cpu-arm64-1.9` in `.env`. The Makefile exports `.env`, so `make up` uses it. If you run `docker compose -f deploy/docker-compose.yml …` directly, export the variable in your shell first, because Compose looks for `.env` next to the compose file, not at the repository root.
- **ERPNext image**: `build-image.sh` builds for the engine's own architecture and fails if the image doesn't match it. To check by hand: `docker image inspect --format '{{.Architecture}}' close-copilot/erpnext:15` should print `arm64`.
- **Compose platform pin**: frappe_docker's `compose.yaml` sets `platform: linux/amd64` on every Frappe service. `deploy/erpnext/compose.local.yaml` removes that pin with `platform: !reset null`, so Docker runs the native image instead of looking for an amd64 one. That is why Compose v2.24 or later is required.
- MariaDB 11.8 and Redis 8.6 publish arm64 images, and wkhtmltopdf has an arm64 package for bookworm. Nothing runs under emulation.

## Regenerate the ERPNext compose file

`deploy/erpnext/docker-compose.yaml` is generated and committed. Don't edit it by hand. Change `deploy/erpnext/compose.local.yaml` (our override) or the submodule pin, then run:

```sh
deploy/erpnext/generate-compose.sh
```

This runs `docker compose config` on frappe_docker's `compose.yaml`, its `compose.mariadb.yaml`, `compose.redis.yaml` and `compose.noproxy.yaml` overrides, and `compose.local.yaml`, with `CUSTOM_IMAGE=close-copilot/erpnext`, `CUSTOM_TAG=15`, `PULL_POLICY=never` and `FRAPPE_SITE_NAME_HEADER=erp.localhost`. It runs in a scrubbed environment, so variables from your shell or `.env` (a `DB_PASSWORD`, say) can't leak into the committed file. The output holds only frappe_docker's development defaults.

## Reset ERPNext

This deletes the site, the MariaDB data and the Redis queue, and everything seeded into them:

```sh
docker compose -f deploy/erpnext/docker-compose.yaml down --volumes
make up
deploy/erpnext/new-site.sh
deploy/erpnext/setup-wizard.sh
```

The volumes are `close-copilot-erpnext_sites`, `close-copilot-erpnext_db-data` and `close-copilot-erpnext_redis-queue-data`. Postgres and the TEI models live in the separate `close-copilot` project and are untouched. One ERPNext is shared by every session, so a reset wipes whatever another session wrote; take `tmp/erpnext.lock` first (see `CLAUDE.md`). Backup and restore come in CC-307.

To reset Postgres too: `docker compose -f deploy/docker-compose.yml down --volumes`. That also deletes the downloaded TEI models.

## Troubleshooting

- **`make up` says `pull access denied` or `No such image: close-copilot/erpnext:15`**: build the image first (step 4). `PULL_POLICY=never` stops Compose from looking for it on Docker Hub.
- **404 on `http://localhost:8080`**: the site doesn't exist yet; run step 6.
- **`new-site.sh` waits on `common_site_config.json`**: the `configurator` container failed. Check `docker compose -f deploy/erpnext/docker-compose.yaml logs configurator db`.
- **Containers restart or the build dies with exit code 137**: Docker is out of memory. Raise the limit in Docker Desktop or stop other containers.
