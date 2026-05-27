"""Unit tests for the security invariants of scripts/embedding-server.py.

Run with:
    .venv/bin/python -m unittest scripts/test_embedding_server.py -v

The embedding server depends on sentence-transformers / torch, which
take seconds to import and ~1 GB of RAM. We stub the module before
loading the server so the tests stay fast and offline.
"""
from __future__ import annotations

import asyncio
import importlib.util
import os
import sys
import types
import unittest
from pathlib import Path
from typing import Optional

import numpy as np


_SOURCE = Path(__file__).resolve().parent / "embedding-server.py"


class _StubModel:
    """Stand-in for sentence_transformers.SentenceTransformer."""

    def __init__(self, name: str, dims: int = 3, raises: Optional[Exception] = None):
        self.name = name
        self.dims = dims
        self.raises = raises

    def encode(self, texts, **_kwargs):
        if self.raises is not None:
            raise self.raises
        return np.array([[1.0, 2.0, 3.0] for _ in texts], dtype=np.float32)

    def get_sentence_embedding_dimension(self):
        return self.dims


def _stub_sentence_transformers(raises: Optional[Exception] = None) -> types.ModuleType:
    module = types.ModuleType("sentence_transformers")

    def factory(name: str):
        return _StubModel(name, raises=raises)

    module.SentenceTransformer = factory  # type: ignore[attr-defined]
    sys.modules["sentence_transformers"] = module
    return module


def _load_server(raises: Optional[Exception] = None):
    """Reload the embedding-server module with a fresh stub. Returns
    the module so tests can poke at _State and call the handlers."""
    _stub_sentence_transformers(raises=raises)
    spec = importlib.util.spec_from_file_location("embedding_server_under_test", _SOURCE)
    assert spec and spec.loader
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class AllowlistTests(unittest.TestCase):
    def test_model_outside_allowlist_returns_400(self):
        srv = _load_server()
        srv._State.default_model = "default-model"
        srv._State.allowed_models = {"default-model"}

        req = srv.EmbedRequest(model="not-allowed", texts=["x"])
        with self.assertRaises(srv.HTTPException) as ctx:
            asyncio.run(srv.embed(req))
        self.assertEqual(ctx.exception.status_code, 400)
        self.assertIn("not-allowed", ctx.exception.detail)
        self.assertIn("allowlist", ctx.exception.detail)

    def test_default_model_is_always_allowed(self):
        srv = _load_server()
        srv._State.default_model = "default-model"
        srv._State.allowed_models = {"default-model"}

        # No explicit model -> falls through to default.
        req = srv.EmbedRequest(texts=["hello"])
        resp = asyncio.run(srv.embed(req))
        self.assertEqual(resp.model, "default-model")
        self.assertEqual(resp.dimensions, 3)
        self.assertEqual(len(resp.embeddings), 1)

    def test_explicitly_allowlisted_model_accepted(self):
        srv = _load_server()
        srv._State.default_model = "default-model"
        srv._State.allowed_models = {"default-model", "extra-model"}

        req = srv.EmbedRequest(model="extra-model", texts=["a", "b"])
        resp = asyncio.run(srv.embed(req))
        self.assertEqual(resp.model, "extra-model")
        self.assertEqual(len(resp.embeddings), 2)


class InputCapTests(unittest.TestCase):
    def test_too_many_texts_rejected_by_pydantic(self):
        from pydantic import ValidationError

        srv = _load_server()
        with self.assertRaises(ValidationError):
            srv.EmbedRequest(texts=["x"] * (srv.MAX_TEXTS_PER_REQUEST + 1))

    def test_text_too_long_rejected_by_pydantic(self):
        from pydantic import ValidationError

        srv = _load_server()
        with self.assertRaises(ValidationError):
            srv.EmbedRequest(texts=["x" * (srv.MAX_TEXT_BYTES + 1)])

    def test_at_limits_accepted(self):
        srv = _load_server()
        # Exactly at the caps should pass validation.
        req = srv.EmbedRequest(
            texts=["x" * srv.MAX_TEXT_BYTES] * srv.MAX_TEXTS_PER_REQUEST
        )
        self.assertEqual(len(req.texts), srv.MAX_TEXTS_PER_REQUEST)

    def test_model_name_length_capped(self):
        from pydantic import ValidationError

        srv = _load_server()
        with self.assertRaises(ValidationError):
            srv.EmbedRequest(model="x" * (srv.MAX_MODEL_NAME_BYTES + 1), texts=["a"])


class GenericErrorMessageTests(unittest.TestCase):
    def test_internal_failure_returns_generic_message(self):
        secret = "AWS_SECRET_ACCESS_KEY=hunter2 in /tmp/leaky.log"
        srv = _load_server(raises=RuntimeError(secret))
        srv._State.default_model = "model"
        srv._State.allowed_models = {"model"}

        req = srv.EmbedRequest(model="model", texts=["x"])
        with self.assertRaises(srv.HTTPException) as ctx:
            asyncio.run(srv.embed(req))
        self.assertEqual(ctx.exception.status_code, 500)
        self.assertEqual(ctx.exception.detail, "internal server error")
        self.assertNotIn(secret, str(ctx.exception.detail))


if __name__ == "__main__":
    # Allow `python scripts/test_embedding_server.py`.
    unittest.main(verbosity=2)
