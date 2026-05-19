#!/usr/bin/env python3
"""Local embedding server for docsearch semantic search.

Contract (matches internal/semanticindex/embedder.go):

    POST /embed
    Request:  {"model": "<hf-model-id>", "texts": ["...", ...]}
    Response: {"model": "<hf-model-id>", "dimensions": N, "embeddings": [[float, ...], ...]}

Default model: BAAI/bge-small-en-v1.5 (384-dim, English-only retrieval).

Usage:
    python -m venv .venv && . .venv/bin/activate
    pip install -r scripts/requirements-embedding-server.txt
    python scripts/embedding-server.py        # serves on http://127.0.0.1:8000/embed

Models are downloaded from Hugging Face on first use and cached under
$HF_HOME (default: ~/.cache/huggingface).
"""
from __future__ import annotations

import argparse
import json
import logging
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Lock

DEFAULT_MODEL = "BAAI/bge-small-en-v1.5"
DEFAULT_HOST = "127.0.0.1"
DEFAULT_PORT = 8000

_model_lock = Lock()
_models: dict[str, object] = {}


def get_model(name: str):
    from sentence_transformers import SentenceTransformer

    with _model_lock:
        model = _models.get(name)
        if model is None:
            logging.info("loading model %s", name)
            model = SentenceTransformer(name)
            _models[name] = model
        return model


class Handler(BaseHTTPRequestHandler):
    server_version = "docsearch-embed/0.1"

    def _json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if self.path.rstrip("/") == "/healthz":
            self._json(200, {"status": "ok", "models": sorted(_models)})
            return
        self._json(404, {"error": f"unknown path {self.path}"})

    def do_POST(self) -> None:
        if self.path.rstrip("/") != "/embed":
            self._json(404, {"error": f"unknown path {self.path}"})
            return
        length = int(self.headers.get("Content-Length", "0") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            req = json.loads(raw)
        except json.JSONDecodeError as exc:
            self._json(400, {"error": f"invalid json: {exc}"})
            return

        texts = req.get("texts")
        if not isinstance(texts, list) or not all(isinstance(t, str) for t in texts):
            self._json(400, {"error": "texts must be a list of strings"})
            return
        model_name = req.get("model") or DEFAULT_MODEL

        try:
            model = get_model(model_name)
            vectors = model.encode(
                texts,
                normalize_embeddings=True,
                convert_to_numpy=True,
            ).tolist()
        except Exception as exc:
            logging.exception("embedding failure")
            self._json(500, {"error": str(exc)})
            return

        dims = len(vectors[0]) if vectors else int(model.get_sentence_embedding_dimension())
        self._json(200, {"model": model_name, "dimensions": dims, "embeddings": vectors})

    def log_message(self, format: str, *args) -> None:
        logging.info("%s - " + format, self.address_string(), *args)


def main() -> int:
    parser = argparse.ArgumentParser(description="Local embedding server for docsearch")
    parser.add_argument("--host", default=os.environ.get("HOST", DEFAULT_HOST))
    parser.add_argument("--port", type=int, default=int(os.environ.get("PORT", DEFAULT_PORT)))
    parser.add_argument(
        "--model",
        default=os.environ.get("EMBEDDING_MODEL", DEFAULT_MODEL),
        help="Hugging Face model id to preload at startup",
    )
    args = parser.parse_args()

    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    logging.info("preloading model %s", args.model)
    get_model(args.model)
    logging.info("listening on http://%s:%d/embed (default model=%s)", args.host, args.port, args.model)

    server = HTTPServer((args.host, args.port), Handler)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        logging.info("shutting down")
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
