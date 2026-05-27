# docsearch — install & operate

Self-contained Docker Compose deployment of the Dyalog documentation
search service. Everything is in this directory; you do **not** need
the source repository, a Go or Python toolchain, or any build step.
The images are pre-built and published — you only pull and run them.

## What it is

Three containers behind one HTTP port:

- **proxy** — Caddy. The only published port. Round-robins across the
  web replicas and health-checks them.
- **web** — the search frontend + API. Stateless; the documentation
  database is baked into the image. Runs 2 replicas by default.
- **embedder** — the model that turns a search query into a vector.
  One replica serves the whole user base.

The service is plain HTTP. Put it behind your existing TLS-terminating
reverse proxy / load balancer.

## Requirements

- A Linux host (x86-64 or arm64 — multi-arch images are published).
- Docker Engine with the Compose plugin (`docker compose version`).
- Outbound HTTPS to `ghcr.io` to pull the images (first run only).
- ~7 GB free disk for the images; ~2.5 GB RAM headroom for the
  embedder.
- `curl` and `python3` on the host if you want to run `verify.sh`.

## Install

```bash
# 1. (optional) review/override defaults
cp .env.example .env        # then edit if needed; defaults work as-is

# 2. pull the published images
docker compose pull

# 3. start the stack
docker compose up -d

# 4. confirm it works end-to-end
./verify.sh
```

`verify.sh` exits `0` and prints `verify: OK` when the proxy, the
database, the embedder, and the search ranking are all healthy. If it
fails it tells you which of the three checks broke.

Then point your reverse proxy at this host on port **8080** (or
whatever `HOST_PORT` you set in `.env`).

## Day-two operations

**Check what's deployed**

```bash
curl -s http://localhost:8080/api/version
docker compose ps
```

**Update to the latest docs** (new images are published weekly)

```bash
docker compose pull
docker compose up -d
./verify.sh
```

**Pin / roll back to a specific build**

Set `TAG=2026-05-27` (any published dated tag) in `.env`, then:

```bash
docker compose pull
docker compose up -d
```

The database is baked into the image, so the docs content rolls back
together with the code — no separate data migration.

**Scale the web frontend** (stateless, safe to do live)

```bash
docker compose up -d --scale web=5
```

**Logs**

```bash
docker compose logs --since 1h web      # access log, one JSON line per request
docker compose logs --tail=50 embedder
```

**Stop / restart**

```bash
docker compose down                     # stop and remove containers
docker compose up -d                    # bring back up
```

## Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `docker compose pull` → `denied` / `not found` | Image isn't public, or wrong `REGISTRY`/`TAG` | Confirm the GHCR packages are public and `.env` matches the published namespace/tag |
| `verify.sh`: `/api/health returned no body` | Stack not up yet, or proxy port blocked | `docker compose ps`; check the host firewall on `HOST_PORT` |
| `verify.sh`: search retries then fails | Embedder still loading its model (cold start ~10 s) | Wait and re-run; if it persists, `docker compose logs embedder` |
| Searches return results but ranking looks off | Stale image | `docker compose pull && docker compose up -d` |

For deeper triage (reading the access log, embedder stalls, error
message reference) the maintainers keep a fuller runbook with the
source.
