"""Local Laya sidecar exposing the Jev/OpenRouter Decisions API shape.

POST /decisions  {"model": ..., "state": {...} | "...", "questions": {...}}
             ->  {"model": ..., "answers": {...}, "usage": {"input_tokens": n, "cost": 0}}

model: "laya" (English, 512 tok) | "laya-multilingual" (1024 tok, 100+ langs)
       | "laya-auto" (pick by detected language, like laya.Router)
Weights come from the convaiinnovations/laya Hugging Face repo (safetensors, ~1.6 GB per checkpoint).

Run: .venv/bin/python laya_server.py --port 8788 [--preload] [--pad-buckets 64,128,256,512,1024]
"""
import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import laya
import laya.agent as _laya_agent
import torch

CHECKPOINTS = {"laya": None, "laya-multilingual": "multilingual"}
_agents, _lock = {}, threading.Lock()
DEVICE = None  # None = auto (cuda > mps > cpu)
PRELOAD = False

# --- Bucketed padding -------------------------------------------------------
#
# On MPS, torch recompiles kernels whenever the input tensor shape changes, which makes a call
# with a never-before-seen sequence length cost 200-350ms instead of the usual 30-70ms (see
# README "Latency"). laya.agent.Agent.system_one() batches all questions for one call through
# laya.common.collate_items(), which pads every item to the *exact* max length in that batch -
# a length that varies with every request. Rounding that length up to a small, fixed set of
# bucket sizes makes the shape repeat across calls, so MPS only ever compiles a handful of
# kernels.
#
# This is safe because collate_items() already produces an attention_mask, and the encoder
# (laya.common.DecisionModel.forward) uses it both for the transformer encoder's own attention
# and for the extra decision-head layers' src_key_padding_mask - padded positions never
# contribute to any output. Verified empirically: with vs. without bucket padding, on 12 varied
# prompts through the English checkpoint on MPS, the maximum absolute difference in any reported
# probability was 0.0 (bit-identical), and no answer (choice/score/noul) ever differed.
#
# Implementation: collate_items is imported by name into laya.agent's module namespace
# (`from .common import (..., collate_items, ...)`), and Python resolves that name from the
# module's globals at call time - so replacing `laya.agent.collate_items` here is enough to
# intercept every call from `Agent.system_one`, with no need to fork or patch the laya package.
DEFAULT_PAD_BUCKETS = [64, 128, 256, 512, 1024]
_ORIG_COLLATE_ITEMS = _laya_agent.collate_items
PAD_BUCKETS = []  # set by install_padding(); empty = disabled
_force_bucket = None  # set only during warmup, to land exactly on one bucket


def parse_buckets(spec: str):
    """Parse '--pad-buckets' into a sorted list of ints, or [] for 'none' / empty."""
    if spec is None or spec.strip().lower() in ("none", ""):
        return []
    try:
        buckets = sorted({int(x) for x in spec.split(",") if x.strip()})
    except ValueError as e:
        raise ValueError(f"invalid --pad-buckets value {spec!r}: {e}") from e
    if any(b <= 0 for b in buckets):
        raise ValueError(f"invalid --pad-buckets value {spec!r}: bucket sizes must be positive")
    return buckets


def _bucket_for(length: int, buckets):
    for b in buckets:
        if length <= b:
            return b
    return None  # longer than the largest bucket: leave it at its natural (unbucketed) length


def _padded_collate_items(batch, pad_id):
    b = _ORIG_COLLATE_ITEMS(batch, pad_id)
    if b is None:
        return b
    target = _force_bucket if _force_bucket is not None else _bucket_for(b["input_ids"].shape[1], PAD_BUCKETS)
    if target is None:
        return b
    n, length = b["input_ids"].shape
    pad_len = target - length
    if pad_len > 0:
        b["input_ids"] = torch.cat(
            [b["input_ids"], torch.full((n, pad_len), pad_id, dtype=b["input_ids"].dtype)], dim=1)
        b["attention_mask"] = torch.cat(
            [b["attention_mask"], torch.zeros((n, pad_len), dtype=b["attention_mask"].dtype)], dim=1)
    return b


def install_padding(buckets):
    """Enable bucket padding. Call once at startup; buckets=[] leaves collate_items untouched."""
    global PAD_BUCKETS
    PAD_BUCKETS = buckets
    if buckets:
        _laya_agent.collate_items = _padded_collate_items


def warm_buckets(a, buckets):
    """Run one dummy call per bucket <= this checkpoint's max_len, so MPS compiles every
    bucket's kernel before the first real request instead of on it."""
    global _force_bucket
    max_len = a.cfg.get("max_len", 512)
    q = {"q": {"type": "noul", "instructions": "Is this a warmup call?"}}
    for bucket in buckets:
        if bucket > max_len:
            continue
        _force_bucket = bucket
        try:
            t0 = time.time()
            a.system_one("warmup", q)
            print(f"[laya] warmed bucket {bucket} in {time.time() - t0:.3f}s", flush=True)
        finally:
            _force_bucket = None


def agent(name):
    with _lock:  # one model instance per checkpoint, inference serialized (MPS is not reentrant)
        if name not in _agents:
            t0 = time.time()
            a = laya.load("convaiinnovations/laya", subfolder=CHECKPOINTS[name], device=DEVICE)
            _agents[name] = a
            print(f"[laya] loaded {name} on {a.device} in {time.time() - t0:.1f}s", flush=True)
            if PRELOAD and PAD_BUCKETS:
                warm_buckets(a, PAD_BUCKETS)
        return _agents[name]


def resolve(model, state):
    model = (model or "laya-auto").split("/")[-1]
    if model in ("laya-auto", "auto"):
        return "laya" if laya.detect_language(state)["is_english"] else "laya-multilingual"
    if model in ("laya", "laya-english", "english"):
        return "laya"
    if model in ("laya-multilingual", "multilingual"):
        return "laya-multilingual"
    raise ValueError(f"unknown model {model!r}")


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/healthz":
            return self._send(200, {"ok": True, "loaded": list(_agents), "pad_buckets": PAD_BUCKETS})
        self._send(404, {"error": "not found"})

    def do_POST(self):
        if self.path not in ("/decisions", "/v1/decisions", "/api/alpha/decisions"):
            return self._send(404, {"error": "not found"})
        try:
            req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            name = resolve(req.get("model"), req["state"])
            a = agent(name)
            with _lock:
                t0 = time.perf_counter()
                out = a.system_one(req["state"], req["questions"])
                ms = (time.perf_counter() - t0) * 1000
            out["model"] = f"convaiinnovations/{name}"
            out["provider"] = "laya-local"
            out["usage"]["cost"] = 0
            out["usage"]["inference_ms"] = round(ms, 1)
            self._send(200, out)
        except Exception as e:  # noqa: BLE001 - surface any failure to the caller
            self._send(400, {"error": {"message": f"{type(e).__name__}: {e}"}})

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8788)
    ap.add_argument("--device", default=None, help="cpu | mps | cuda (default: auto)")
    ap.add_argument("--preload", action="store_true", help="load both checkpoints at startup")
    ap.add_argument(
        "--pad-buckets", default="64,128,256,512,1024",
        help="comma-separated sequence-length buckets to pad inputs to, so repeated shapes avoid "
             "MPS recompiles (default: 64,128,256,512,1024); 'none' disables padding",
    )
    args = ap.parse_args()
    DEVICE = args.device
    PRELOAD = args.preload
    install_padding(parse_buckets(args.pad_buckets))
    print(f"[laya] pad buckets: {PAD_BUCKETS or 'disabled'}", flush=True)
    if args.preload:
        for n in CHECKPOINTS:
            agent(n)
    print(f"[laya] listening on http://{args.host}:{args.port}/decisions", flush=True)
    ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()
