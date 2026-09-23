// Package config loads the gateway configuration (config.yaml).
package config

import (
	"bufio"
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    string            `yaml:"listen"`
	LogPath   string            `yaml:"log_path"`
	Upstream  Upstream          `yaml:"upstream"`
	Decision  Decision          `yaml:"decision"`
	Routing   Routing           `yaml:"routing"`
	Topics    map[string]string `yaml:"topics"`
	Models    []Model           `yaml:"models"`
	Anthropic Anthropic         `yaml:"anthropic"`
}

// Anthropic configures the Anthropic Messages API endpoint (POST /v1/messages).
type Anthropic struct {
	// AutoModels are glob patterns (path.Match) of model names routed like "auto", e.g. ["claude-*"]
	// so clients with hard-coded model names (Claude Code) get routed too.
	AutoModels []string `yaml:"auto_models"`
}

type Upstream struct {
	BaseURL       string `yaml:"base_url"`
	APIKeyEnv     string `yaml:"api_key_env"`
	RefreshPrices bool   `yaml:"refresh_prices"`
}

type Decision struct {
	// Provider is "jev", "laya" or "auto" (auto: laya for private requests when available, jev otherwise).
	Provider            string                      `yaml:"provider"`
	Shadow              string                      `yaml:"shadow"`  // second provider run in background for agreement logging
	Private             string                      `yaml:"private"` // prefer_local | local_only | ignore
	ConfidenceThreshold float64                     `yaml:"confidence_threshold"`
	RiskOffset          float64                     `yaml:"risk_offset"`
	Providers           map[string]DecisionProvider `yaml:"providers"`
}

type DecisionProvider struct {
	URL           string        `yaml:"url"`
	Model         string        `yaml:"model"`
	APIKeyEnv     string        `yaml:"api_key_env"`
	Local         bool          `yaml:"local"`
	MaxStateChars int           `yaml:"max_state_chars"`
	Timeout       time.Duration `yaml:"timeout"`
}

type Routing struct {
	MinSkill        []float64     `yaml:"min_skill"`         // indexed by complexity 0..3
	RiskBonus       []float64     `yaml:"risk_bonus"`        // added to min skill, indexed by risk 0..2
	EstOutputTokens []int         `yaml:"est_output_tokens"` // indexed by complexity
	LoadPenalty     float64       `yaml:"load_penalty"`      // cost multiplier per in-flight request
	StickyTTL       time.Duration `yaml:"sticky_ttl"`
	FallbackModel   string        `yaml:"fallback_model"`
	// Explore is the probability (0 = off) of trying the next-cheaper capable model just below the quality
	// floor on easy, harmless, non-private requests, so cmd/refit gets outcomes for models that rarely win.
	Explore float64 `yaml:"explore"`
	// ReasoningEffort by complexity (e.g. [low, low, medium, high]); applied only when the client sets none.
	ReasoningEffort []string `yaml:"reasoning_effort"`
	// Retries: on 429/5xx from upstream, try the next-best candidate this many times.
	Retries int   `yaml:"retries"`
	Check   Check `yaml:"check"`
}

// Check asks the decision model whether a non-streaming answer is good enough, and re-sends the
// request to a stronger model when it is not.
type Check struct {
	Enabled        bool    `yaml:"enabled"`
	Threshold      float64 `yaml:"threshold"`        // P(answer ok) below this escalates
	MaxAnswerChars int     `yaml:"max_answer_chars"` // answer trimmed to this in the decision state
	MinComplexity  int     `yaml:"min_complexity"`   // only check requests at least this complex
}

type Model struct {
	ID             string             `yaml:"id"`
	Price          Price              `yaml:"price"` // USD per million tokens
	Context        int                `yaml:"context"`
	Tools          bool               `yaml:"tools"`
	Vision         bool               `yaml:"vision"`
	Local          bool               `yaml:"local"`
	BaseURL        string             `yaml:"base_url"`    // optional: overrides upstream.base_url (e.g. a local Ollama/LM Studio server)
	APIKeyEnv      string             `yaml:"api_key_env"` // optional: overrides upstream.api_key_env
	DefaultSkill   float64            `yaml:"default_skill"`
	Skills         map[string]float64 `yaml:"skills"` // per-topic affinity 0..1
	DailyBudgetUSD float64            `yaml:"daily_budget_usd"`
	// Anthropic marks a model with its own base_url as serving the Anthropic Messages API (/messages).
	// Models without base_url go through upstream.base_url (OpenRouter), which serves it for every model.
	Anthropic bool `yaml:"anthropic"`
	// OutputMultiplier scales expected output tokens for cost estimates (reasoning models think out loud).
	OutputMultiplier float64 `yaml:"output_multiplier"`
}

type Price struct {
	In  float64 `yaml:"in"`
	Out float64 `yaml:"out"`
}

func (m Model) Skill(topic string) float64 {
	if s, ok := m.Skills[topic]; ok {
		return s
	}
	return m.DefaultSkill
}

// ServesAnthropic reports whether the model can take Anthropic Messages API requests.
func (m Model) ServesAnthropic() bool { return m.BaseURL == "" || m.Anthropic }

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c := &Config{}
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c.defaults()
	return c, c.validate()
}

func (c *Config) defaults() {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8787"
	}
	if c.LogPath == "" {
		c.LogPath = "data/decisions.jsonl"
	}
	if c.Decision.Provider == "" {
		c.Decision.Provider = "jev"
	}
	if c.Decision.Private == "" {
		c.Decision.Private = "prefer_local"
	}
	if c.Decision.ConfidenceThreshold == 0 {
		c.Decision.ConfidenceThreshold = 0.8
	}
	for k, p := range c.Decision.Providers {
		if p.MaxStateChars == 0 {
			p.MaxStateChars = 4000
		}
		if p.Timeout == 0 {
			p.Timeout = 3 * time.Second
		}
		c.Decision.Providers[k] = p
	}
	if len(c.Routing.MinSkill) == 0 {
		c.Routing.MinSkill = []float64{0.3, 0.5, 0.7, 0.85}
	}
	if len(c.Routing.RiskBonus) == 0 {
		c.Routing.RiskBonus = []float64{0, 0.05, 0.1}
	}
	if len(c.Routing.EstOutputTokens) == 0 {
		c.Routing.EstOutputTokens = []int{300, 800, 2000, 4000}
	}
	for i := range c.Models {
		if c.Models[i].OutputMultiplier == 0 {
			c.Models[i].OutputMultiplier = 1
		}
	}
	if c.Routing.Check.Threshold == 0 {
		c.Routing.Check.Threshold = 0.5
	}
	if c.Routing.Check.MaxAnswerChars == 0 {
		c.Routing.Check.MaxAnswerChars = 3000
	}
	if c.Routing.StickyTTL == 0 {
		c.Routing.StickyTTL = 2 * time.Hour
	}
}

func (c *Config) validate() error {
	if len(c.Models) == 0 {
		return fmt.Errorf("no models configured")
	}
	if len(c.Topics) == 0 {
		return fmt.Errorf("no topics configured")
	}
	if c.Decision.Provider != "auto" {
		if _, ok := c.Decision.Providers[c.Decision.Provider]; !ok {
			return fmt.Errorf("decision provider %q not defined", c.Decision.Provider)
		}
	}
	for _, p := range c.Anthropic.AutoModels {
		if _, err := path.Match(p, ""); err != nil {
			return fmt.Errorf("anthropic.auto_models: bad pattern %q", p)
		}
	}
	for _, m := range c.Models {
		for t := range m.Skills {
			if _, ok := c.Topics[t]; !ok {
				return fmt.Errorf("model %s: unknown topic %q", m.ID, t)
			}
		}
	}
	if c.Routing.FallbackModel != "" && c.Model(c.Routing.FallbackModel) == nil {
		return fmt.Errorf("fallback_model %q not in models", c.Routing.FallbackModel)
	}
	return nil
}

func (c *Config) Model(id string) *Model {
	for i := range c.Models {
		if c.Models[i].ID == id {
			return &c.Models[i]
		}
	}
	return nil
}

// LoadDotEnv sets variables from a KEY=VALUE file without overriding the existing environment.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
}
