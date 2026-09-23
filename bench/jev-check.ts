// Validates the Jev routing assumptions against a labelled prompt set.
// Run: node --env-file=.env bench/jev-check.ts [--baseline] [--model typesafe/jev-1.13]
import { writeFileSync, mkdirSync } from "node:fs";
import { CASES, TOPICS, VERIFY_CASES, type Case, type Topic } from "./cases.ts";

const KEY = process.env.OPENROUTER_KEY ?? process.env.OPENROUTER_API_KEY;
if (!KEY) throw new Error("OPENROUTER_KEY missing (run with --env-file=.env)");

const args = process.argv.slice(2);
const JEV_MODEL = args.includes("--model") ? args[args.indexOf("--model") + 1] : "typesafe/jev-1.13";
const BASELINE_MODEL = "deepseek/deepseek-v4.1-flash";
const DECISIONS_URL = "https://openrouter.ai/api/alpha/decisions";
const CHAT_URL = "https://openrouter.ai/api/v1/chat/completions";
const topicKeys = Object.keys(TOPICS) as Topic[];

const COMPLEXITY = [
  "Trivial: one-liner or quick fact, no reasoning needed",
  "Simple: small self-contained task, a competent junior could do it",
  "Substantial: multi-step reasoning or debugging across several parts",
  "Hard: architectural, large-scope, or expert-level reasoning",
];
const RISK = [
  "Harmless: mistakes have no real consequence",
  "Moderate: a wrong answer could break code or waste time",
  "High: security, production outage, data loss, or legal exposure",
];

// ---------- helpers ----------
async function post(url: string, body: unknown) {
  const t0 = performance.now();
  const res = await fetch(url, {
    method: "POST",
    headers: { Authorization: `Bearer ${KEY}`, "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
  const ms = performance.now() - t0;
  const text = await res.text();
  if (!res.ok) throw new Error(`${res.status} ${text.slice(0, 500)}`);
  return { json: JSON.parse(text), ms };
}

async function pool<T, R>(items: T[], n: number, fn: (t: T) => Promise<R>): Promise<R[]> {
  const out: R[] = new Array(items.length);
  let i = 0;
  await Promise.all(Array.from({ length: n }, async () => {
    while (i < items.length) { const k = i++; out[k] = await fn(items[k]); }
  }));
  return out;
}

const pct = (xs: number[], p: number) => { const s = [...xs].sort((a, b) => a - b); return s[Math.min(s.length - 1, Math.floor(p * s.length))]; };
const sum = (xs: number[]) => xs.reduce((a, b) => a + b, 0);
const fmt = (n: number, d = 1) => n.toFixed(d);
const pctStr = (a: number, b: number) => `${a}/${b} (${fmt((100 * a) / b, 0)}%)`;

// ---------- Jev router call ----------
function routerQuestions() {
  const q: Record<string, unknown> = {
    primary_topic: { type: "choice", instructions: "What is the main kind of work this request asks for?", criteria: TOPICS },
    complexity: { type: "score", instructions: "How much reasoning capability does this request need?", criteria: COMPLEXITY },
    risk: { type: "score", instructions: "How costly would a wrong or low-quality answer be?", criteria: RISK },
    private_data: {
      type: "noul", instructions: "Does the request contain secrets or personal/confidential data?",
      criteria: { true: "Contains credentials, keys, personal identifiers, salaries, customer PII, or confidential data.", false: "No sensitive data is present." },
    },
  };
  for (const t of topicKeys) q[`topic_${t}`] = {
    type: "noul", instructions: `Is "${t}" a relevant part of this request?`,
    criteria: { true: TOPICS[t], false: "This kind of work is not meaningfully involved." },
  };
  return q;
}

interface Routed { primary: Topic; primaryConf: number; topics: Record<Topic, number>; complexity: number; risk: number; privateP: number; ms: number; cost: number; inTok: number; }

async function jevRoute(c: Case): Promise<Routed> {
  const { json, ms } = await post(DECISIONS_URL, { model: JEV_MODEL, state: { request: c.prompt }, questions: routerQuestions() });
  const a = json.answers;
  const topics = Object.fromEntries(topicKeys.map((t) => [t, a[`topic_${t}`].noul])) as Record<Topic, number>;
  return {
    primary: a.primary_topic.choice, primaryConf: a.primary_topic.confidence ?? a.primary_topic.probabilities?.[a.primary_topic.choice],
    topics, complexity: a.complexity.score, risk: a.risk.score, privateP: a.private_data.noul,
    ms, cost: json.usage?.cost ?? 0, inTok: json.usage?.input_tokens ?? 0,
  };
}

// ---------- LLM baseline (same questions, JSON out) ----------
async function llmRoute(c: Case): Promise<Routed> {
  const sys = `You are a request router. Classify the user request. Reply ONLY with JSON:
{"primary_topic": one of ${JSON.stringify(topicKeys)},
 "topics": {${topicKeys.map((t) => `"${t}": 0..1`).join(", ")}},
 "complexity": 0..3, "risk": 0..2, "private_data": 0..1, "confidence": 0..1}
Topics: ${JSON.stringify(TOPICS)}
Complexity levels: ${JSON.stringify(COMPLEXITY)}
Risk levels: ${JSON.stringify(RISK)}`;
  const { json, ms } = await post(CHAT_URL, {
    model: BASELINE_MODEL, temperature: 0, response_format: { type: "json_object" }, usage: { include: true },
    messages: [{ role: "system", content: sys }, { role: "user", content: c.prompt }],
  });
  const raw = json.choices[0].message.content.replace(/^```(json)?|```$/g, "");
  const a = JSON.parse(raw);
  return {
    primary: a.primary_topic, primaryConf: a.confidence ?? 1, topics: a.topics, complexity: a.complexity, risk: a.risk,
    privateP: a.private_data, ms, cost: json.usage?.cost ?? 0, inTok: json.usage?.prompt_tokens ?? 0,
  };
}

// ---------- scoring ----------
function evaluate(name: string, rs: (Routed | Error)[]) {
  const ok = rs.map((r, i) => [CASES[i], r] as const).filter((x): x is readonly [Case, Routed] => !(x[1] instanceof Error));
  const errors = rs.filter((r) => r instanceof Error) as Error[];
  let prim = 0, cxExact = 0, cxNear = 0, rkExact = 0, priv = 0, tp = 0, fp = 0, fn = 0;
  const calib: { conf: number; hit: boolean }[] = [];
  const misses: string[] = [];
  for (const [c, r] of ok) {
    const hit = r.primary === c.primary; prim += +hit; calib.push({ conf: r.primaryConf, hit });
    const cx = Math.round(r.complexity), rk = Math.round(r.risk);
    cxExact += +(cx === c.complexity); cxNear += +(Math.abs(cx - c.complexity) <= 1); rkExact += +(rk === c.risk);
    priv += +((r.privateP >= 0.5) === c.private);
    for (const t of topicKeys) {
      const pred = (r.topics[t] ?? 0) >= 0.5, gold = c.topics.includes(t);
      tp += +(pred && gold); fp += +(pred && !gold); fn += +(!pred && gold);
    }
    if (!hit || Math.abs(cx - c.complexity) > 1 || rk !== c.risk)
      misses.push(`  ${c.id.padEnd(14)} primary ${String(r.primary).padEnd(13)}(${c.primary}) conf=${fmt(r.primaryConf ?? 0, 2)}  cx ${fmt(r.complexity, 2)}(${c.complexity})  risk ${fmt(r.risk, 2)}(${c.risk})`);
  }
  const n = ok.length, lat = ok.map(([, r]) => r.ms), cost = ok.map(([, r]) => r.cost);
  const prec = tp / (tp + fp || 1), rec = tp / (tp + fn || 1);
  const hi = calib.filter((x) => x.conf >= 0.8), lo = calib.filter((x) => x.conf < 0.8);
  console.log(`\n=== ${name} ===  (${n} ok, ${errors.length} errors)`);
  console.log(`primary topic        ${pctStr(prim, n)}`);
  console.log(`multi-label topics   precision ${fmt(prec * 100, 0)}%  recall ${fmt(rec * 100, 0)}%  F1 ${fmt((200 * prec * rec) / (prec + rec || 1), 0)}%`);
  console.log(`complexity           exact ${pctStr(cxExact, n)}   within ±1 ${pctStr(cxNear, n)}`);
  console.log(`risk                 exact ${pctStr(rkExact, n)}`);
  console.log(`private data         ${pctStr(priv, n)}`);
  console.log(`calibration          conf≥0.8: ${pctStr(hi.filter((x) => x.hit).length, hi.length || 1)} correct   conf<0.8: ${pctStr(lo.filter((x) => x.hit).length, lo.length || 1)} correct`);
  console.log(`latency              p50 ${fmt(pct(lat, 0.5), 0)}ms  p90 ${fmt(pct(lat, 0.9), 0)}ms`);
  console.log(`cost                 total $${sum(cost).toFixed(6)}  per call $${(sum(cost) / n).toFixed(7)}  → per 1000 routes $${((1000 * sum(cost)) / n).toFixed(4)}`);
  if (misses.length) console.log(`misses / off-by>1:\n${misses.join("\n")}`);
  errors.slice(0, 3).forEach((e) => console.log(`  error: ${e.message}`));
  return { name, n, errors: errors.length, primary: prim / n, f1: (2 * prec * rec) / (prec + rec || 1), cxExact: cxExact / n, cxNear: cxNear / n, risk: rkExact / n, priv: priv / n, p50: pct(lat, 0.5), p90: pct(lat, 0.9), costPer1k: (1000 * sum(cost)) / n };
}

// ---------- verify (check-and-escalate) ----------
async function verify() {
  const rs = await pool(VERIFY_CASES, 4, async (v) => {
    const { json, ms } = await post(DECISIONS_URL, {
      model: JEV_MODEL, state: { request: v.request, answer: v.answer },
      questions: { satisfied: { type: "noul", instructions: "Does the answer correctly and completely satisfy the request?", criteria: { true: "The answer is correct and addresses the request.", false: "The answer is wrong, incomplete, or off-topic." } } },
    });
    return { ...v, p: json.answers.satisfied.noul as number, ms, cost: json.usage?.cost ?? 0 };
  });
  const hits = rs.filter((r) => (r.p >= 0.5) === r.good).length;
  console.log(`\n=== Jev verify (check-and-escalate) ===`);
  rs.forEach((r) => console.log(`  ${r.id.padEnd(10)} P(satisfied)=${fmt(r.p, 2)}  expected ${r.good ? "yes" : "no "}  ${(r.p >= 0.5) === r.good ? "✓" : "✗"}  ${fmt(r.ms, 0)}ms`));
  console.log(`accuracy ${pctStr(hits, rs.length)}`);
  return { accuracy: hits / rs.length };
}

// ---------- context scaling ----------
async function scaling() {
  console.log(`\n=== Jev context scaling (latency/cost vs state size) ===`);
  const filler = "The service logs show routine health checks passing and nothing unusual. ";
  const out = [];
  for (const reps of [0, 50, 400, 1600]) {
    const prompt = `Debug why the payment worker crashes with OOM.\n\nLogs:\n${filler.repeat(reps)}`;
    const { json, ms } = await post(DECISIONS_URL, {
      model: JEV_MODEL, state: { request: prompt },
      questions: { primary_topic: { type: "choice", instructions: "What is the main kind of work this request asks for?", criteria: TOPICS } },
    });
    const a = json.answers.primary_topic;
    console.log(`  ~${String(json.usage?.input_tokens).padStart(6)} tok  ${fmt(ms, 0).padStart(5)}ms  $${(json.usage?.cost ?? 0).toFixed(6)}  → ${a.choice} (${fmt(a.confidence, 2)})`);
    out.push({ tokens: json.usage?.input_tokens, ms, cost: json.usage?.cost, choice: a.choice });
  }
  return out;
}

// ---------- main ----------
const wrap = <T,>(p: Promise<T>) => p.catch((e: Error) => e);
console.log(`Jev model: ${JEV_MODEL}   cases: ${CASES.length}`);
const jev = evaluate(`Jev router (${JEV_MODEL})`, await pool(CASES, 4, (c) => wrap(jevRoute(c))));
const report: Record<string, unknown> = { date: new Date().toISOString(), jev };
if (args.includes("--baseline")) report.baseline = evaluate(`LLM router baseline (${BASELINE_MODEL})`, await pool(CASES, 4, (c) => wrap(llmRoute(c))));
report.verify = await verify();
report.scaling = await scaling();
mkdirSync("bench/results", { recursive: true });
const file = `bench/results/jev-${Date.now()}.json`;
writeFileSync(file, JSON.stringify(report, null, 2));
console.log(`\nSaved ${file}`);
