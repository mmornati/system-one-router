// Topic taxonomy and labelled prompts used to validate the routing assumptions.
import { readFileSync } from "node:fs";
// complexity: 0 trivial · 1 simple · 2 substantial · 3 hard/architectural
// risk:       0 harmless · 1 could break things · 2 security/prod/data-loss

export const TOPICS = {
  "code-gen": "Writing new code, functions, scripts, or features.",
  "code-review": "Reviewing existing code or a diff for quality, bugs, or style.",
  "debugging": "Diagnosing an error, crash, failing test, or unexpected behaviour.",
  "security": "Vulnerabilities, secrets, auth, permissions, or attack surface.",
  "docs": "Writing or updating documentation, READMEs, changelogs, comments.",
  "data-sql": "Databases, SQL queries, schemas, migrations, data analysis.",
  "infra-devops": "CI/CD, Docker, Kubernetes, cloud, deployment, monitoring.",
  "architecture": "System design, trade-offs, planning larger technical changes.",
  "writing": "Non-code prose: emails, blog posts, meeting notes, summaries.",
  "chat": "Small talk, quick factual questions, or simple how-to answers.",
} as const;

export type Topic = keyof typeof TOPICS;

export interface Case {
  id: string;
  group: string;
  lang: string;
  prompt: string;
  primary: Topic;
  topics: Topic[]; // all relevant topics (includes primary)
  complexity: 0 | 1 | 2 | 3;
  risk: 0 | 1 | 2;
  private: boolean; // contains secrets / personal / confidential data
}

// Labelled prompts live in cases.json (shared with the Go benchmark, cmd/bench).
export const CASES: Case[] = JSON.parse(readFileSync(new URL("./cases.json", import.meta.url), "utf8"));

// Check-and-escalate: does a cheap model's answer satisfy the request?
export const VERIFY_CASES = [
  { id: "ok-regex", request: "Write a JS function that returns true if a string is a palindrome.", answer: "function isPal(s){ const c=s.toLowerCase().replace(/[^a-z0-9]/g,''); return c===[...c].reverse().join(''); }", good: true },
  { id: "bad-regex", request: "Write a JS function that returns true if a string is a palindrome.", answer: "function isPal(s){ return s.length > 0; }", good: false },
  { id: "ok-sql", request: "SQL to count orders per customer in table orders(customer_id, id).", answer: "SELECT customer_id, COUNT(*) AS n FROM orders GROUP BY customer_id;", good: true },
  { id: "bad-sql", request: "SQL to count orders per customer in table orders(customer_id, id).", answer: "SELECT COUNT(*) FROM orders;", good: false },
  { id: "offtopic", request: "Explain how to rotate a leaked AWS key.", answer: "AWS is a cloud provider founded in 2006 offering many services like S3 and EC2.", good: false },
  { id: "ok-rotate", request: "Explain how to rotate a leaked AWS key.", answer: "1) Create a new access key for the IAM user. 2) Update apps to use it. 3) Deactivate then delete the leaked key. 4) Check CloudTrail for misuse and review billing.", good: true },
];
