# docsearch — docker compose deployment

Compose stack for serving docsearch to a small group of users
(target: double-digit concurrent users, no Kubernetes, no shared
state). Three services:

```
  proxy   caddy on host:8080 → round-robins to all web replicas
  web     stateless. docsearch + sqlite-vec + dyalog-docs.db baked
          in. scale freely with --scale web=N
  embedder  fastapi + sentence-transformers + preloaded model. one
            replica handles ~100 users with debounced typing.
```

The database is read-only at runtime and is baked into the web
image, so containers have no volumes, no shared state, and no
consistency model to worry about. Updating the docs is an image
rebuild + redeploy.

## Layout

| File | Role |
|---|---|
| `Dockerfile.web` | Multi-stage: builds docsearch with `-tags "fts5 semantic"`, installs sqlite-vec, COPYs the prebuilt DB |
| `Dockerfile.embedder` | Python image with sentence-transformers + the FastAPI server. Model weights downloaded at build time |
| `compose.yaml` | Three-service stack with healthchecks, replica counts, and resource limits |
| `Caddyfile` | Round-robin reverse proxy with `/api/health`-based active health checks |
| `build-db.sh` | Run on the host: produces `dyalog-docs.db` via `bundle-docs` + `semantic-index` |
| `build-images.sh` | Build both images with buildx (defaults to `linux/arm64`) |

## First-time setup

```bash
# 1. Local toolchain — only needed for the DB build.
python -m venv .venv
.venv/bin/pip install -r scripts/requirements-embedding-server.txt

# 2. Build the DB (clones the docs repo, runs the embedder, indexes).
#    Output: deploy/dyalog-docs.db
./deploy/build-db.sh

# 3. Build the images.
./deploy/build-images.sh

# 4. Start the stack.
cd deploy && docker compose up -d
```

Browse <http://localhost:8080>.

## Scaling

The `web` service is stateless and trivially horizontally
scalable. Bring more replicas up at any time:

```bash
docker compose up -d --scale web=5
```

Caddy picks up the new replicas via Docker's embedded DNS and starts
round-robining within seconds. Bring it back down with `--scale web=2`.

For the target user count (10-99) a single embedder is plenty: with
~250 ms debounce on the client and ~100 ms per inference, one
embedder serves roughly 10 queries per second sustained. If `/embed`
latency starts to back up, raise `WEB_REPLICAS` is not enough — you
need more embedders. Bump `deploy.replicas` on the `embedder`
service in `compose.yaml` and put a small Caddy upstream in front of
it (same pattern as the web service).

## Docs updates

A few times a year:

```bash
./deploy/build-db.sh                  # fresh DB from upstream
TAG=$(date +%F) ./deploy/build-images.sh
TAG=$(date +%F) docker compose up -d --force-recreate
```

Tagging by date keeps the rollback story trivial: `TAG=2026-04-12
docker compose up -d` to revert.

## Pushing to a registry

```bash
PUSH=1 REGISTRY=ghcr.io/xpqz TAG=2026-05-21 ./deploy/build-images.sh
```

then on the target host:

```bash
REGISTRY=ghcr.io/xpqz TAG=2026-05-21 docker compose pull
REGISTRY=ghcr.io/xpqz TAG=2026-05-21 docker compose up -d
```

## Why the DB is baked in

- **Read-only & low churn.** A handful of updates per year and no
  strong-consistency requirement makes shared mutable state the wrong
  tool.
- **Stateless replicas.** No volumes, no init containers, no
  "container A on the new index, container B on the old" mid-deploy.
  A replica is fully defined by its image tag.
- **Trivial rollback.** Deploy a prior image tag; the DB rolls back
  with it.
- **Negligible image-size cost.** `dyalog-docs.db` is ~15 MB after
  semantic indexing; even five replicas only adds ~75 MB across all
  containers, and Docker share layers when images differ only at
  the top.

## Why one embedder

The sentence-transformers model isn't thread-safe on the same
device, so each embedder process serializes inference internally
anyway. Running a single embedder behind multiple web frontends
keeps one ~1 GB process resident instead of N. The web replicas can
queue concurrent requests across the embedder without blocking
their own HTTP loops thanks to the `AbortController`-aware client
in the UI.

If you ever hit the embedder ceiling, scale by adding embedder
replicas behind a small Caddy upstream (mirror the `web` setup).

## Architecture support

`build-images.sh` defaults to `linux/arm64` (the development host
is Apple Silicon). Multi-arch images:

```bash
PLATFORMS=linux/arm64,linux/amd64 PUSH=1 ./deploy/build-images.sh
```

Note: the sqlite-vec install script inside `Dockerfile.web` detects
host architecture via `uname` so it pulls the right `vec0.so` for
whichever platform is being built.

## What's intentionally not here

- **No persistent volumes.** The DB is read-only. The embedder's
  model cache lives inside the image. Logs go to stdout for Docker
  to collect.
- **No TLS.** Caddy is configured as a plain-HTTP local proxy. If
  you want public HTTPS, drop `auto_https off` from the Caddyfile
  and set a hostname.
- **No external auth.** This is a docs reader for a known small
  group of users; if that ever changes, put it behind your existing
  SSO or an oauth2-proxy sidecar.
