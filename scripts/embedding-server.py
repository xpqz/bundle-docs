#!/usr/bin/env python3
"""Local embedding server for docsearch (FastAPI + uvicorn).

Contract (matches internal/semanticindex/embedder.go):

    POST /embed
    Request:  {"model": "<hf-model-id>", "texts": ["...", ...]}
    Response: {"model": "<hf-model-id>", "dimensions": N,
               "embeddings": [[float, ...], ...]}

Also:

    GET  /healthz     liveness  - always 200 once the process is up
    GET  /readyz      readiness - 200 once the model is loaded, 503 before

Default model: BAAI/bge-small-en-v1.5 (384-dim, English-only retrieval).

Usage:
    python -m venv .venv && . .venv/bin/activate
    pip install -r scripts/requirements-embedding-server.txt
    python scripts/embedding-server.py        # http://127.0.0.1:8000/embed

Inference is dispatched to a single worker thread because the underlying
PyTorch model is not safe to run concurrently on the same MPS device.
HTTP handling stays on the asyncio event loop so request parsing and
connection management overlap with inference; this is the real win
over the previous stdlib HTTPServer, which serialized everything down
to the listening socket.
"""
from __future__ import annotations

import argparse
import asyncio
import logging
import os
import sys
from concurrent.futures import ThreadPoolExecutor
from contextlib import asynccontextmanager
from threading import Lock
from typing import Optional

import uvicorn
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel, Field

DEFAULT_MODEL = "BAAI/bge-small-en-v1.5"
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8000

logger = logging.getLogger("embedding-server")

# Single-worker executor: sentence-transformers / PyTorch are not safe
# to run concurrently on the same device, especially MPS. Queueing
# requests behind one worker keeps inference deterministic while still
# letting the asyncio loop accept and parse new connections.
_inference_pool = ThreadPoolExecutor(max_workers=1, thread_name_prefix="infer")
_model_lock = Lock()
_models: dict[str, object] = {}


class _State:
    default_model: str = DEFAULT_MODEL
    ready: bool = False


def _load_model(name: str):
    """Load the named SentenceTransformer model, caching by name."""
    from sentence_transformers import SentenceTransformer

    with _model_lock:
        model = _models.get(name)
        if model is None:
            logger.info("loading model %s", name)
            model = SentenceTransformer(name)
            _models[name] = model
        return model


def _encode(name: str, texts: list[str]):
    model = _load_model(name)
    vectors = model.encode(
        texts,
        normalize_embeddings=True,
        convert_to_numpy=True,
    ).tolist()
    if vectors:
        dims = len(vectors[0])
    else:
        dims = int(model.get_sentence_embedding_dimension())
    return dims, vectors


@asynccontextmanager
async def lifespan(_: FastAPI):
    logger.info("preloading model %s", _State.default_model)
    loop = asyncio.get_event_loop()
    await loop.run_in_executor(_inference_pool, _load_model, _State.default_model)
    _State.ready = True
    logger.info("ready; listening for requests")
    try:
        yield
    finally:
        logger.info("shutting down")
        _inference_pool.shutdown(wait=False, cancel_futures=True)


app = FastAPI(
    title="docsearch embedding server",
    version="0.2",
    lifespan=lifespan,
)


class EmbedRequest(BaseModel):
    model: Optional[str] = None
    texts: list[str] = Field(default_factory=list)


class EmbedResponse(BaseModel):
    model: str
    dimensions: int
    embeddings: list[list[float]]


@app.post("/embed", response_model=EmbedResponse)
async def embed(req: EmbedRequest) -> EmbedResponse:
    name = req.model or _State.default_model
    if not req.texts:
        return EmbedResponse(model=name, dimensions=0, embeddings=[])
    try:
        dims, vectors = await asyncio.get_event_loop().run_in_executor(
            _inference_pool, _encode, name, list(req.texts)
        )
    except Exception as exc:
        logger.exception("embedding failure for model=%s n=%d", name, len(req.texts))
        raise HTTPException(status_code=500, detail=str(exc))
    return EmbedResponse(model=name, dimensions=dims, embeddings=vectors)


@app.get("/healthz")
async def healthz() -> dict:
    return {"status": "ok"}


@app.get("/readyz")
async def readyz() -> dict:
    if not _State.ready:
        raise HTTPException(status_code=503, detail="model still loading")
    return {"status": "ready", "models": sorted(_models)}


def main() -> int:
    parser = argparse.ArgumentParser(description="Local embedding server for docsearch")
    parser.add_argument("--host", default=os.environ.get("HOST", DEFAULT_HOST))
    parser.add_argument("--port", type=int, default=int(os.environ.get("PORT", DEFAULT_PORT)))
    parser.add_argument(
        "--model",
        default=os.environ.get("EMBEDDING_MODEL", DEFAULT_MODEL),
        help="Hugging Face model id to preload at startup",
    )
    parser.add_argument(
        "--log-level",
        default=os.environ.get("LOG_LEVEL", "info"),
        choices=["critical", "error", "warning", "info", "debug"],
    )
    args = parser.parse_args()
    _State.default_model = args.model

    logging.basicConfig(
        level=args.log_level.upper(),
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )

    # workers=1: the model is held in memory and not fork-safe on MPS.
    # If you need more throughput, scale horizontally with multiple
    # processes behind a reverse proxy, not multiple uvicorn workers.
    uvicorn.run(
        app,
        host=args.host,
        port=args.port,
        log_level=args.log_level,
        access_log=False,  # disable per-request noise; structured access logs are easy to add later
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
