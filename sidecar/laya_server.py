"""Local Laya sidecar exposing the Jev/OpenRouter Decisions API shape.

POST /decisions  {"model": ..., "state": {...} | "...", "questions": {...}}
             ->  {"model": ..., "answers": {...}, "usage": {"input_tokens": n, "cost": 0}}

model: "laya" (English, 512 tok) | "laya-multilingual" (1024 tok, 100+ langs)
       | "laya-auto" (pick by detected language, like laya.Router)
Weights come from the convaiinnovations/laya Hugging Face repo (safetensors, ~1.6 GB per checkpoint).

Run: .venv/bin/python laya_server.py --port 8788 [--preload]
"""
import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import laya

CHECKPOINTS = {"laya": None, "laya-multilingual": "multilingual"}
_agents, _lock = {}, threading.Lock()
DEVICE = None  # None = auto (cuda > mps > cpu)


def agent(name):
    with _lock:  # one model instance per checkpoint, inference serialized (MPS is not reentrant)
        if name not in _agents:
            t0 = time.time()
            _agents[name] = laya.load("convaiinnovations/laya", subfolder=CHECKPOINTS[name], device=DEVICE)
            print(f"[laya] loaded {name} on {_agents[name].device} in {time.time() - t0:.1f}s", flush=True)
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
            return self._send(200, {"ok": True, "loaded": list(_agents)})
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
    args = ap.parse_args()
    DEVICE = args.device
    if args.preload:
        for n in CHECKPOINTS:
            agent(n)
    print(f"[laya] listening on http://{args.host}:{args.port}/decisions", flush=True)
    ThreadingHTTPServer((args.host, args.port), Handler).serve_forever()
