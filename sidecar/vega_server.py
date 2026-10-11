"""Local VegaML sidecar exposing the Jev/OpenRouter Decisions API shape.

POST /v1/systemone  {"model": ..., "state": {...} | "...", "questions": {...}}
                ->  {"model": ..., "answers": {...}, "usage": {"input_tokens": n, "cost": 0}}

VegaML (github.com/NandhaKishorM/vegaml, Apache-2.0) is a frozen Qwen3.5 backbone plus a small trained
"physics engine". It speaks typed questions like Jev, so this only renames fields:

    request  "noul"  -> vegaml "boolean"            response "boolean"        -> "noul"
    response "probs" -> "probabilities"             response "expected_score" -> "score" (like Jev/Laya)

VegaML's own fields (abstain, prediction_set, unbound, readout, ...) are passed through untouched.

--fit FILE fits VegaML's per-question "TTT" heads from labelled examples before serving
(JSON lines: {"state": ..., "labels": {question_id: label}, "questions": {...}}), so mode "auto"
answers each fitted question with its head. cmd/bench uses it for the cross-validated few-shot run.

Run: .venv-vega/bin/python vega_server.py --port 8793 [--size 0.8b] [--mode engine] [--fit examples.jsonl]
"""
import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import vegaml

_lock = threading.Lock()  # one model, inference serialized (MPS is not reentrant)
V = None
ARGS = None


def to_vega(questions):
    out = {}
    for k, q in questions.items():
        q = dict(q)
        if q.get("type") == "noul":
            q["type"] = "boolean"
        out[k] = q
    return out


def from_vega(answers, questions):
    out = {}
    for k, a in answers.items():
        t = questions[k].get("type")
        r = {kk: vv for kk, vv in a.items() if kk not in ("probs", "boolean", "score", "expected_score", "choice")}
        r["type"] = t
        r["probabilities"] = a["probs"]
        r["confidence"] = a["confidence"]
        if t == "choice":
            r["choice"] = a["choice"]
        elif t == "score":
            r["score"] = round(a["expected_score"], 4)
            r["level"] = a["score"]
        else:  # noul / boolean
            r["noul"] = a["boolean"]
        out[k] = r
    return out


def fit(path):
    examples, questions = [], None
    with open(path) as f:
        for line in f:
            if line.strip():
                ex = json.loads(line)
                examples.append((ex["state"], ex["labels"]))
                questions = to_vega(ex["questions"])
    t0 = time.time()
    report = V.fit(examples, questions)
    print(f"[vega] fitted {len(examples)} examples in {time.time() - t0:.1f}s", flush=True)
    for qid, r in report.items():
        print(f"[vega]   {qid}: fitted={r.get('fitted')} cv={r.get('cv_accuracy')} chance={r.get('chance')} "
              f"auto={r.get('used_by_auto')} {r.get('reason', '')}", flush=True)
    return report


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
            return self._send(200, {"ok": True, "size": ARGS.size, "mode": ARGS.mode, "heads": sorted(V.heads)})
        self._send(404, {"error": "not found"})

    def do_POST(self):
        if self.path not in ("/v1/systemone", "/decisions", "/v1/decisions", "/api/alpha/decisions"):
            return self._send(404, {"error": "not found"})
        try:
            req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            qs = req["questions"]
            with _lock:
                t0 = time.perf_counter()
                out = V.decide(req["state"], to_vega(qs), mode=ARGS.mode, views=ARGS.views)
                ms = (time.perf_counter() - t0) * 1000
            self._send(200, {
                "model": f"vegaml-{ARGS.size}", "provider": "vega-local", "mode": out.get("mode"),
                "answers": from_vega(out["answers"], qs),
                "usage": {"input_tokens": out.get("n_tokens", 0), "cost": 0, "inference_ms": round(ms, 1)},
            })
        except Exception as e:  # noqa: BLE001 - surface any failure to the caller
            self._send(400, {"error": {"message": f"{type(e).__name__}: {e}"}})

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=8793)
    ap.add_argument("--size", default="0.8b", help="0.8b | 4b | a Hub repo id or local path")
    ap.add_argument("--device", default="mps", help="cpu | mps | cuda (vegaml itself defaults to cuda > cpu)")
    ap.add_argument("--mode", default="engine", choices=["engine", "auto"],
                    help="engine = zero-shot, what VegaML's published figures measure; auto = fitted heads where they beat chance")
    ap.add_argument("--views", type=int, default=1, help="1-3 readings averaged per decision (about views x the cost)")
    ap.add_argument("--fit", default=None, help="JSON-lines labelled examples to fit TTT heads on before serving")
    ARGS = ap.parse_args()
    t0 = time.time()
    V = vegaml.load(ARGS.size, device=ARGS.device, mode=ARGS.mode)
    print(f"[vega] loaded {ARGS.size} on {ARGS.device} in {time.time() - t0:.1f}s (mode={ARGS.mode})", flush=True)
    if ARGS.fit:
        fit(ARGS.fit)
    print(f"[vega] listening on http://{ARGS.host}:{ARGS.port}/v1/systemone", flush=True)
    ThreadingHTTPServer((ARGS.host, ARGS.port), Handler).serve_forever()
