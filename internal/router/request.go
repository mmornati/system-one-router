package router

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Request is a provider-agnostic summary of a chat request, enough to route it.
type Request struct {
	System    string
	FirstUser string
	LastUser  string
	UserTurns int
	Chars     int
	Tools     bool
	Vision    bool
	// AnthropicAPI is set for Anthropic Messages API requests: only models that serve that API qualify.
	AnthropicAPI bool
	// Rest is every other text the model reads (earlier turns, tool calls and results): scanned by the
	// privacy pre-check, never sent to the decision model.
	Rest string
}

// StickyKey identifies a conversation: same system prompt + same opening message.
// Keeping a conversation on one model preserves prompt caching and consistency.
func (r Request) StickyKey() string {
	h := sha256.Sum256([]byte(r.System + "\x00" + r.FirstUser))
	return hex.EncodeToString(h[:12])
}

func (r Request) EstTokens() int { return r.Chars/4 + 1 }

// State builds the decision-model state, trimmed to maxChars (decision models have small contexts:
// Laya's English checkpoint only sees 512 tokens).
func (r Request) State(maxChars int) map[string]string {
	st := map[string]string{"request": r.LastUser}
	if r.UserTurns > 1 && r.FirstUser != r.LastUser {
		st["conversation_start"] = r.FirstUser
	}
	if r.System != "" {
		st["system_prompt"] = r.System
	}
	budget := maxChars
	for _, k := range []string{"request", "conversation_start", "system_prompt"} {
		v, ok := st[k]
		if !ok {
			continue
		}
		share := budget
		if k == "request" && len(st) > 1 {
			share = budget * 2 / 3
		}
		st[k] = trimMiddle(v, share)
		budget -= len(st[k])
		if budget <= 0 {
			budget = 0
		}
	}
	for k, v := range st {
		if v == "" {
			delete(st, k)
		}
	}
	return st
}

func trimMiddle(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 20 {
		return s[:max(n, 0)]
	}
	head := n * 2 / 3
	return s[:head] + " … " + s[len(s)-(n-head-3):]
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                        // AWS access key
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),                 // OpenAI / OpenRouter style keys
	regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),            // GitHub tokens
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),      // PEM private keys
	regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`),                   // US SSN
	regexp.MustCompile(`\b[A-Z]{2}\d{2}(?: ?[A-Z0-9]{4}){3,7}\b`), // IBAN
}

// LooksPrivate is a cheap local pre-check, run before any data leaves the machine.
func (r Request) LooksPrivate() bool {
	return looksPrivate(r.System + "\n" + r.FirstUser + "\n" + r.LastUser + "\n" + r.Rest)
}

func looksPrivate(all string) bool {
	for _, p := range secretPatterns {
		if p.MatchString(all) {
			return true
		}
	}
	return strings.Contains(strings.ToLower(all), "password:")
}
