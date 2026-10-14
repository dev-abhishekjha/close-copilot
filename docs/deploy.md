# Deployment on a VM

How to run Close Copilot at a public HTTPS address (CC-1201), and how a reviewer gets the same state on a laptop. Run every command from the repository root. All data is synthetic.

The demo serves pre-computed runs to viewers. Only the maker (`asha`) and checker (`ravi`) accounts can start live runs or approve, and live runs stop at the daily budget cap (`LLM_DAILY_BUDGET_USD`).

## What runs where

`deploy/prod/docker-compose.yml` (project `close-copilot-prod`) includes the ERPNext stack from `deploy/erpnext/docker-compose.yaml` through `deploy/prod/erpnext.override.yaml`, which removes ERPNext's published port 8080.

| Service | Networks | Published port |
| --- | --- | --- |
| `caddy` (automatic HTTPS, reverse proxy to `agent:8000`) | `edge`, `internal` | 80, 443 |
| `agent` (`agent serve`: web UI and API) | `internal`, `egress` | none |
| `mcp-books` (`:7001`), `mcp-evidence` (`:7002`) | `internal`, `egress` | none |
| `postgres` (pgvector, pg17), `migrate` (goose, runs once) | `internal` | none |
| `tei-embed` (`bge-small-en-v1.5`), `tei-rerank` (`bge-reranker-base`) | `internal` | none |
| ERPNext (`frontend`, `backend`, `websocket`, queues, `db`, redis) | its own internal network, plus `frontend` on `internal` | none |
| `docling` (profile `ingest` only) | `internal`, `egress` | none |

The `internal` network is `internal: true`: containers on it have no route to the internet. Only caddy publishes ports. The `egress` network exists because a few services must call out: the agent (Anthropic API, Langfuse Cloud), the MCP servers (Langfuse Cloud spans) and docling (model downloads, only under the `ingest` profile). The TEI servers have no egress at runtime: they run with `HF_HUB_OFFLINE=1` after their models are pulled once (step 3b). ERPNext has no egress at all. The MCP servers are never proxied by caddy.

## 1. The VM

- 4 vCPU, 16 GB RAM, 60 GB disk, Ubuntu LTS. ERPNext takes about 3 GB, Postgres 0.3 GB, TEI about 1 GB, the Go services under 1 GB; docling (about 2 GB) runs only while ingesting. Traces go to Langfuse Cloud, so no Langfuse runs on the VM.
- Firewall: allow inbound 22, 80 and 443 only. Docker publishes ports around `ufw`, so the compose files publish nothing else (the two temporary seeding overrides bind to `127.0.0.1`).
- DNS: an `A` record for your demo host name pointing at the VM, before the first start (caddy needs it to get a certificate).
- Docker Engine 24 or later with the Compose plugin v2.24 or later (the files use `!reset` and `!override`), `git`, `make`, `jq`, Go 1.26+ (seeding runs from a checkout).

Install Docker with the official convenience steps for your distribution (https://docs.docker.com/engine/install/ubuntu/) and add your user to the `docker` group.

Never run `make up` or `make up-ingest` on the VM: they start the development stack, which publishes ports on every interface and uses development passwords. Use the `docker compose` commands below.

## 2. Clone and configure

```sh
git clone --recurse-submodules <repo-url> close_copilot
cd close_copilot
cp deploy/prod/.env.prod.example deploy/prod/.env.prod
chmod 600 deploy/prod/.env.prod
```

Edit `deploy/prod/.env.prod`. Generate every secret fresh with `openssl rand -hex 32`, never reuse values from a local `.env`, and do not commit this file (it is git-ignored).

| Variable | Notes |
| --- | --- |
| `DEMO_DOMAIN` | The public host name |
| `POSTGRES_PASSWORD` | Fresh value |
| `ERP_API_KEY`, `ERP_API_SECRET` | The `copilot-bot` keys, from step 3 |
| `ERP_POST_API_KEY`, `ERP_POST_API_SECRET` | The `copilot-poster` keys, from step 3. Only `mcp-books` receives them; it refuses to start if they equal the bot's keys |
| `MCP_TOKEN_AGENT`, `MCP_TOKEN_ADMIN`, `MCP_SCOPE_KEY` | Fresh, all different |
| `PSEUDONYM_KEY` | Fresh |
| `APP_SESSION_KEY` | At least 32 bytes |
| `LLM_PROVIDER` | `anthropic` |
| `ANTHROPIC_API_KEY` | A key made for this demo. In the Anthropic console, also set a monthly spend limit on its workspace |
| `LLM_MODEL_FAST`, `LLM_MODEL_STRONG` | Full model IDs from Anthropic's models page |
| `LLM_DAILY_BUDGET_USD` | Keep it low (2.00). Raise it only with the owner |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_HEADERS` | Langfuse Cloud: create a project, copy its public and secret keys, set the header to `Authorization=Basic <base64(public_key:secret_key)>` |

Never set `COPILOT_FAULT` or `FAULTS` on the VM. Prompts are never traced (`TRACE_PROMPTS` is fixed to `false` in the compose file).

## 3. Bring-up order

Compose reads `deploy/prod/.env.prod` through `--env-file`. These shell variables keep the commands short:

```sh
PROD="-f deploy/prod/docker-compose.yml --env-file deploy/prod/.env.prod"
```

### 3a. ERPNext first-time setup

The ERPNext scripts drive the `close-copilot-erpnext` project, so create the site with that stack, with its port bound to `127.0.0.1` through `deploy/prod/erpnext.seed.yaml`. Its volumes are named explicitly, so the prod stack reuses the same data afterwards. Set `ERP_ADMIN_PASSWORD` to a fresh value first (it applies when the site is created; the MariaDB root password stays at the development default because the database is never reachable from outside the ERPNext network).

```sh
export ERP_ADMIN_PASSWORD="$(openssl rand -hex 16)"
deploy/erpnext/build-image.sh                      # 15 to 30 minutes, once
docker compose -f deploy/erpnext/docker-compose.yaml -f deploy/prod/erpnext.seed.yaml up -d
deploy/erpnext/new-site.sh
deploy/erpnext/setup-wizard.sh
deploy/erpnext/api-users.sh                        # writes tmp/erp-keys.env
```

Copy the bot and poster keys from `tmp/erp-keys.env` into `deploy/prod/.env.prod` (open the file; do not print it into shared logs). The seeder's Administrator keys stay in `tmp/erp-keys.env`.

### 3b. Seed, load and ingest

Start Postgres, the TEI servers and the migration with their ports on `127.0.0.1` through `deploy/prod/docker-compose.seed.yml`. This override also gives TEI temporary egress and turns `HF_HUB_OFFLINE` off, so the two models download once into the `tei-models` volume (the pre-pull step). Wait until both have logged that they are ready, then check with `docker compose $PROD logs tei-embed tei-rerank`. The prod stack never reaches Hugging Face:

```sh
docker compose $PROD -f deploy/prod/docker-compose.seed.yml up -d postgres migrate tei-embed tei-rerank
docker compose $PROD -f deploy/prod/docker-compose.seed.yml --profile ingest up -d docling
```

Create a `.env` for `make` (git-ignored) from `.env.example` with these local-facing values, the same `POSTGRES_PASSWORD`, and the same `PSEUDONYM_KEY`:

```sh
ERP_BASE_URL=http://localhost:8080
DATABASE_URL=postgres://copilot:<POSTGRES_PASSWORD>@localhost:5432/copilot?sslmode=disable
TEI_EMBED_URL=http://localhost:8081
TEI_RERANK_URL=http://localhost:8082
DOCLING_URL=http://localhost:5001
```

Then seed ERPNext (the seeder uses the Administrator keys, so run it in a subshell as `docs/setup.md` and `.env.example` describe), take the clean backup, load and ingest:

```sh
(set -a; . tmp/erp-keys.env; set +a; go run ./cmd/seed bootstrap)
deploy/erpnext/backup-clean.sh
(set -a; . tmp/erp-keys.env; set +a; make erp-reset seed load ingest SUITE=suite-v1)
```

Docling is only needed for `ingest`.

### 3c. Rotate the web logins (owner step, before anything public starts)

The bcrypt hashes in `config/users.yaml` are development values, and the image copies that file in at build time. Replace each with a hash of a fresh password (for example `htpasswd -bnBC 12 "" '<password>' | tr -d ':\n'`) and commit it. Do not start caddy until this is done and the image is built from that commit.

### 3d. Switch to the prod stack

```sh
docker compose -f deploy/erpnext/docker-compose.yaml down        # keeps the volumes
docker compose $PROD -f deploy/prod/docker-compose.seed.yml --profile ingest down
docker compose $PROD up -d --build
docker compose $PROD stop scheduler                              # saves memory; nothing in the demo needs it
docker compose $PROD ps
```

The first start downloads the two TEI models, which takes a few minutes. caddy gets its certificate on the first request to `https://<DEMO_DOMAIN>`. Check that only caddy listens publicly: `docker compose $PROD ps --format json | jq -r '.Publishers[]? | select(.PublishedPort>0) | .URL'` lists nothing but caddy's addresses.

### 3e. Pre-compute one close run per month

Viewers see stored runs, so make one per company and month while signed in as `asha`, then approve or leave pending as you want viewers to see it. Runs use the Anthropic key and count against `LLM_DAILY_BUDGET_USD`; if a run reports the budget is reached, wait for the next day or raise the cap with the owner.

## 4. Logins

Users live in `config/users.yaml`, and the image copies that file in at build time.

| User | Role | Who gets it |
| --- | --- | --- |
| `demo` | viewer | Shared publicly. It can read; it cannot start a run, approve or reject, because `internal/web/routes.go` allows `POST /runs` to makers only and approve and reject to checkers only (`internal/web/routes_test.go`) |
| `asha` | maker | Share privately with interviewers |
| `ravi` | checker | Share privately with interviewers |

There is no `DEMO_MODE` switch; the read-only behaviour comes from the viewer role.

The hashes must be rotated before the public stack starts: see step 3c.

## 5. Day-two operations

- **Logs:** `docker compose $PROD logs -f agent`.
- **Update:** `git pull`, `docker compose $PROD up -d --build`. The `migrate` service applies new migrations first.
- **Backups:** Postgres: `docker compose $PROD exec -T postgres pg_dump -U copilot copilot | gzip > copilot-$(date +%F).sql.gz`. ERPNext: `deploy/erpnext/backup-clean.sh` for the clean state, or `docker compose $PROD exec backend bench --site erp.localhost backup`. Copy backups off the VM; they hold synthetic data only. The `results` volume holds the run reports.
- **Teardown:** `docker compose $PROD --profile ingest down` stops everything and keeps data. Add `-v` to delete the prod volumes (Postgres, TEI models, runs, caddy certificates); ERPNext's volumes are named `close-copilot-erpnext_*` and are removed with `docker volume rm`.
- **Secrets rotation:** edit `deploy/prod/.env.prod` and run `docker compose $PROD up -d`. Rotating `POSTGRES_PASSWORD` also needs `ALTER USER copilot PASSWORD '...'` in Postgres, because the variable only applies when the volume is created.

## Reviewer path

To reproduce the state on a laptop (about 10 GB of Docker memory; see `docs/setup.md` for prerequisites and the one-time ERPNext site setup):

```sh
git clone --recurse-submodules <repo-url> close_copilot
cd close_copilot
cp .env.example .env
```

Fill in the keys in `.env` as `.env.example` describes, build and set up ERPNext once as `docs/setup.md` steps 4 to 7 and `deploy/erpnext/api-users.sh` do, then:

```sh
make up erp-reset seed load ingest
make run-agent
```

`make migrate` applies the migrations if the tables are missing. The app serves on `http://localhost:8000` (`APP_ADDR`) with the same three logins; with `LLM_PROVIDER=claude-cli` the model calls go through your local `claude -p` and need no API key.

## Owner checklist

These steps are not automated: provision the VM, set DNS, put real secrets in `deploy/prod/.env.prod`, set the Anthropic console limit, rotate the user hashes, run the closes, then confirm that the demo URL loads over HTTPS with the `demo` login and that a fresh clone reaches the same state by following this file alone.
