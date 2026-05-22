# docsearch operational runbook

Recipes for the situations you'll actually see in production. Each
section is self-contained — read whichever one matches the problem.

All commands assume you're in the project root, with the docker
compose stack defined in `deploy/compose.yaml`.

## Table of contents

- [What's currently deployed?](#whats-currently-deployed)
- [Refresh the documentation](#refresh-the-documentation)
- [Roll back to a previous build](#roll-back-to-a-previous-build)
- [Fresh deploy on a new host](#fresh-deploy-on-a-new-host)
- [Smoke-test a deploy](#smoke-test-a-deploy)
- [User reports "search is broken"](#user-reports-search-is-broken)
- [Embedder seems stuck or slow](#embedder-seems-stuck-or-slow)
- [Reading the access log](#reading-the-access-log)
- [Scaling up under load](#scaling-up-under-load)
- [Common error messages](#common-error-messages)

---

## What's currently deployed?

Two equivalent answers, depending on whether you can reach the
running stack or only the host:

```sh
curl -s http://<host>:<port>/api/version | jq
```

returns the build SHA, build timestamp, Go version, and the upstream
Dyalog docs ref (`docs_ref`) that the baked-in DB was produced
against. Same data is in any web replica via:

```sh
docker compose exec web docsearch version
```

If the `web` containers are running but unreachable from outside,
inspect the image tag instead:

```sh
docker compose ps web --format '{{.Image}}'
```

## Refresh the documentation

Run once after the upstream Dyalog docs repo gets a meaningful
change (typically a few times a year).

```sh
make refresh TAG=$(date +%Y-%m-%d)        # tip of main
make refresh TAG=docs-abc123 DOCS_REF=abc123  # pinned to a commit
```

`make refresh` is `make db` (Docker-only DB build) + `make images`
(build web + embedder images) + `make restart` (docker compose
up -d --force-recreate). About 7 minutes on Apple Silicon.

After it finishes, run `make verify` to confirm the new stack
answers the canonical ⎕IO smoke search correctly.

## Roll back to a previous build

Every image is tagged at build time (`TAG=2026-04-12` etc.). To
revert:

```sh
TAG=2026-04-12 docker compose up -d --force-recreate
```

That brings up the older `docsearch-web` (with its baked-in DB) and
the older `docsearch-embedder` against the original embedding model.
The DB is image-baked so the rollback covers data and code in one
step.

Find available tags:

```sh
docker image ls | grep docsearch
```

or, if pushed to a registry,

```sh
docker buildx imagetools inspect ghcr.io/<owner>/docsearch-web:latest
```

## Fresh deploy on a new host

```sh
git clone https://github.com/xpqz/bundle-docs
cd bundle-docs
make refresh        # ~7 minutes; only requires docker + buildx
make verify         # confirms the stack actually works end-to-end
```

The host needs `docker` (with `buildx`) and nothing else: no Go
toolchain, no Python, no sqlite-vec.

If you're deploying from pre-pushed images:

```sh
REGISTRY=ghcr.io/<owner> TAG=2026-05-21 docker compose pull
REGISTRY=ghcr.io/<owner> TAG=2026-05-21 docker compose up -d
make verify
```

## Smoke-test a deploy

```sh
make verify
```

The script (`deploy/verify.sh`) does three things:

1. `GET /api/health` — expects `status` of `ok` or `degraded`.
2. `GET /api/search?q=⎕IO&mode=hybrid&limit=1` — expects the top
   result's title to contain `Index Origin`. Retries up to 5 times
   with 3 s gaps because a freshly-restarted embedder takes ~10 s
   to load its model.
3. `GET /api/version` — expects a non-empty `build_version`.

Failure exits non-zero with a specific diagnostic. Useful as the
last step in any deploy script.

## User reports "search is broken"

1. **Ask which page they were on.** The web UI doesn't have a stable
   URL per search, but they can give you the query they typed.
2. **Ask if they can read the `X-Request-ID` header from their last
   failed request.** In Chrome/Firefox dev tools, the Network tab
   shows it. Most users won't volunteer this; the next steps work
   without it.
3. **Reproduce.** Visit `http://<host>:<port>` and type the same
   query.
4. **If you can reproduce**, look at the most recent access log
   entries for the relevant request:

   ```sh
   docker compose logs web | jq -c 'select(.path == "/api/search")' | tail -20
   ```

   Or, if you have a `req_id`:

   ```sh
   docker compose logs web | jq -c "select(.req_id == \"<id>\")"
   ```

5. **`status: 500`** in those lines → look one entry up for the
   matching error message. Common culprits:
   - `embed query: call embedding service: dial tcp ...: connect:
     connection refused` → the embedder container is down. Skip
     to [Embedder seems stuck or slow](#embedder-seems-stuck-or-slow).
   - `embed query: embedding service returned 400 ... model ... is
     not in the allowlist` → `web` is configured to send a model
     name the embedder doesn't accept. Check
     `docker compose config | grep -i embedding`.
6. **`status: 200` but empty `results`** → the query genuinely
   matched nothing. Use the bare CLI to cross-check:

   ```sh
   docker compose exec web docsearch -s "<their query>" -semantic-mode fts -l 5
   docker compose exec web docsearch -s "<their query>" -semantic-mode hybrid -l 5
   ```

7. **`status: 200`, results present, user still unhappy** → ranking
   regression. Capture the current eval baseline and diff against
   the checked-in `docs/evaluation/semantic-eval-tuned.txt`.

## Embedder seems stuck or slow

The embedder is single-process by design (PyTorch isn't reentrant on
the same device). Symptoms:

- `make verify` reports the `/api/search` retry loop running out.
- Web logs show `embed query: call embedding service: ... context
  deadline exceeded`.
- `docker compose ps embedder` shows it as `(healthy)` but it's not
  responding.

Triage:

```sh
docker compose exec embedder curl -fsS http://localhost:8000/readyz
docker compose logs --tail=50 embedder
```

If `/readyz` is slow or empty, kick it:

```sh
docker compose restart embedder
# wait for it to be healthy again
docker compose ps embedder
```

Web replicas auto-recover thanks to the client-side retry: each
embed call retries up to 3 times with backoff, so a brief embedder
restart is invisible to users.

If it happens repeatedly, check container memory:

```sh
docker stats embedder --no-stream
```

The container has a 2 GiB limit. A loaded `BAAI/bge-small-en-v1.5`
sits around 1 GiB; growth past that suggests a leak or the embedder
loading additional models on demand (which the allowlist should
prevent). Inspect the recent log for unexpected model names.

## Reading the access log

Every web container emits one JSON line per (non-health) request to
stderr, collected by Docker:

```sh
docker compose logs --since 1h web | jq -c 'select(.msg == "request")'
```

Fields:

| Field | Meaning |
|---|---|
| `req_id` | Random per-request id; matches `X-Request-ID` response header |
| `method`, `path`, `query` | What was requested |
| `status` | HTTP status code |
| `bytes` | Response body size |
| `duration_ms` | Wall time inside the handler |
| `remote` | Client IP (prefers `X-Forwarded-For` from the proxy) |

Useful patterns:

```sh
# Slowest requests in the last hour
docker compose logs --since 1h web | jq -c 'select(.msg == "request") | {path, duration_ms, status}' | jq -s 'sort_by(.duration_ms) | reverse | .[:10]'

# All 500s today
docker compose logs --since 24h web | jq -c 'select(.status == 500)'

# Top queries from the access log
docker compose logs --since 24h web | jq -r 'select(.path == "/api/search") | .query' | sort | uniq -c | sort -rn | head -20
```

## Scaling up under load

Two independent levers:

1. **Web replicas.** Stateless — bring up more whenever:

   ```sh
   docker compose up -d --scale web=8
   ```

   Caddy picks them up via DNS within seconds. No effect on the
   embedder; web replicas talk to the same embedder service.

2. **Embedder replicas.** Only worth doing if `/embed` shows up as
   sustained ≥80% busy in `docker stats embedder`. To add embedder
   parallelism, change `embedder.deploy.replicas` in
   `compose.yaml` and add a small reverse-proxy upstream so
   `http://embedder:8000` round-robins between them (mirror the
   `web` pattern).

The web container's memory is capped at 256 MiB and CPU is
unlimited. The embedder's memory cap is 2 GiB.

## Common error messages

| Log line | What it means | What to do |
|---|---|---|
| `docsearch serve: database sanity check failed: docs table is empty` | The `-d` path or the baked DB is wrong. Process refuses to start. | Check `docker compose config` for the web service. Re-run `make refresh`. |
| `embedding model mismatch ... database was indexed with X ... configured -embedding-model: Y` | Embedder is set to a different model than what produced the vectors. Process refuses to start (vector/hybrid would return garbage). | Either set `-embedding-model=X` on docsearch, or re-run `make refresh` so the index uses Y. |
| `WARN: sqlite-vec not loaded ...; falling back to fts-only mode` | sqlite-vec extension missing or unloadable. FTS still works; vector and hybrid will fail. | Rebuild the web image (`make images`); the extension is fetched during build. |
| `embedding service returned 400 ... model X is not in the allowlist` | Web sent a model name the embedder doesn't permit. | Add `--allow-model X` to the embedder service or change `-embedding-model` on web. |
| `panic` (in access log, level=ERROR) | Handler panic. Already returned a clean 500 to the user; investigate the stack trace in the same log line. | File an issue with the stack and the `req_id`. |
| `received shutdown signal, draining in-flight requests` | Process is exiting cleanly on SIGTERM (e.g. `docker compose down`). | Expected during deploys. |
