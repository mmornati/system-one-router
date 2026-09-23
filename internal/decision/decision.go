// Package decision talks to "System One" decision models (TypeSafe Jev on OpenRouter,
// or a local Laya sidecar exposing the same Decisions API shape).
package decision

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Question struct {
	Type         string `json:"type"` // choice | noul | score
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria"` // map[string]string for choice/noul, []string for score
}

func Choice(instr string, options map[string]string) Question {
	return Question{Type: "choice", Instructions: instr, Criteria: options}
}

func Noul(instr, yes, no string) Question {
	return Question{Type: "noul", Instructions: instr, Criteria: map[string]string{"true": yes, "false": no}}
}

func Score(instr string, levels []string) Question {
	return Question{Type: "score", Instructions: instr, Criteria: levels}
}

type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type Result struct {
	Provider    string            `json:"provider"`
	Model       string            `json:"model"`
	Answers     map[string]Answer `json:"answers"`
	InputTokens int               `json:"input_tokens"`
	CostUSD     float64           `json:"cost_usd"`
	Latency     time.Duration     `json:"latency"`
}

type Provider interface {
	Name() string
	Local() bool
	MaxStateChars() int
	Decide(ctx context.Context, state map[string]string, qs map[string]Question) (*Result, error)
}

// HTTPProvider implements the Decisions API: POST {model, state, questions} -> {answers, usage}.
type HTTPProvider struct {
	name, url, model, apiKey string
	local                    bool
	maxState                 int
	timeout                  time.Duration
	client                   *http.Client
}

func NewHTTPProvider(name, url, model, apiKey string, local bool, maxState int, timeout time.Duration) *HTTPProvider {
	return &HTTPProvider{name: name, url: url, model: model, apiKey: apiKey, local: local, maxState: maxState, timeout: timeout, client: &http.Client{}}
}

func (p *HTTPProvider) Name() string       { return p.name }
func (p *HTTPProvider) Local() bool        { return p.local }
func (p *HTTPProvider) MaxStateChars() int { return p.maxState }

func (p *HTTPProvider) Decide(ctx context.Context, state map[string]string, qs map[string]Question) (*Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"model": p.model, "state": state, "questions": qs})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	t0 := time.Now()
	res, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p.name, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d: %.300s", p.name, res.StatusCode, raw)
	}
	var out struct {
		Model   string            `json:"model"`
		Answers map[string]Answer `json:"answers"`
		Usage   struct {
			InputTokens int     `json:"input_tokens"`
			Cost        float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", p.name, err)
	}
	for k := range qs {
		if _, ok := out.Answers[k]; !ok {
			return nil, fmt.Errorf("%s: missing answer %q", p.name, k)
		}
	}
	return &Result{Provider: p.name, Model: out.Model, Answers: out.Answers, InputTokens: out.Usage.InputTokens,
		CostUSD: out.Usage.Cost, Latency: time.Since(t0)}, nil
}
