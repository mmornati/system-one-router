// Package gateway exposes an OpenAI-compatible API and an Anthropic Messages API. Requests with model
// "auto" are routed; any other model name is passed through untouched, so clients can point at the
// gateway blindly.
package gateway

import (
	"bufio"
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/router"
	"github.com/mmornati/system-one-router/internal/stats"
	"github.com/mmornati/system-one-router/internal/store"
	"github.com/mmornati/system-one-router/internal/upstream"
)

//go:embed dashboard.html
var dashboardHTML []byte

const maxBody = 32 << 20

// genID returns a random, time-ordered-enough request id: 12 random bytes as hex.
func genID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

type Server struct {
	Cfg      *config.Config
	Router   *router.Router
	Upstream func(model string) *upstream.Client
	Log      *store.Log
	// LogPath is the JSONL event log path (cfg.LogPath), read separately from Log for /stats.
	LogPath string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /v1/messages", s.messages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.countTokens)
	mux.HandleFunc("POST /route", s.route)
	mux.HandleFunc("POST /feedback", s.feedback)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /stats", s.stats)
	mux.HandleFunc("GET /dashboard", s.dashboard)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

// stats aggregates the event log into dashboard numbers. days=N limits it to the last N days
// (default 7); days=0 means all time.
func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	days := 7
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			httpError(w, http.StatusBadRequest, "days must be a non-negative integer")
			return
		}
		days = n
	}
	var since time.Time
	if days > 0 {
		since = time.Now().UTC().AddDate(0, 0, -days)
	}
	f, err := os.Open(s.LogPath)
	if err != nil {
		if os.IsNotExist(err) {
			st, _ := stats.Compute(bytes.NewReader(nil), s.Cfg, since)
			writeJSON(w, http.StatusOK, st)
			return
		}
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer f.Close()
	st, err := stats.Compute(f, s.Cfg, since)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) dashboard(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(dashboardHTML)
}

func isAuto(model string) bool { return model == "auto" || model == "router/auto" || model == "" }

// route returns the routing decision for a chat request without calling any model (debugging / dry runs).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	_, req, err := readChat(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	id := genID()
	w.Header().Set("X-Router-Request-Id", id)
	d := s.Router.Route(r.Context(), req, id)
	s.Log.Write("route_dry", d)
	writeJSON(w, http.StatusOK, d)
}

// feedback logs a rating for a previously logged request id.
func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Rating  string `json:"rating"`
		Comment string `json:"comment,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.ID == "" || (body.Rating != "good" && body.Rating != "bad") {
		httpError(w, http.StatusBadRequest, `id is required and rating must be "good" or "bad"`)
		return
	}
	s.Log.Write("feedback", map[string]any{"id": body.ID, "rating": body.Rating, "comment": body.Comment})
	w.WriteHeader(http.StatusNoContent)
}

// dialect is one client-facing API: where requests go upstream and how their bodies are read.
type dialect struct {
	name   string                                 // logged as data.api; "" for the OpenAI API
	path   string                                 // upstream path
	openAI bool                                   // set reasoning.effort and usage.include
	usage  func([]byte) usage                     // usage from a response body or one SSE data payload
	text   func([]byte) (string, bool)            // answer text, and whether it is worth checking
	error  func(http.ResponseWriter, int, string) // error body in this API's shape
	// errBody, if set, rewrites a non-SSE upstream error body into this API's shape.
	errBody func(status int, body []byte) []byte
}

var openAIDialect = &dialect{path: "/chat/completions", openAI: true, usage: extractUsage, text: assistantText, error: httpError}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, req, err := readChat(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	model, _ := body["model"].(string)
	s.serve(w, r, openAIDialect, body, req, isAuto(model))
}

// serve routes the request when auto is set (else passes body["model"] through), then forwards it with
// retries, meters it and, for non-streaming routed answers, checks it.
func (s *Server) serve(w http.ResponseWriter, r *http.Request, dl *dialect, body map[string]any, req router.Request, auto bool) {
	id := genID()
	w.Header().Set("X-Router-Request-Id", id)
	model, _ := body["model"].(string)
	attempts := []string{model}
	var d *router.Decision
	if auto {
		d = s.Router.Route(r.Context(), req, id)
		if d.Refused {
			s.Log.Write("refused", d)
			dl.error(w, http.StatusUnprocessableEntity, "request refused by routing policy: "+d.Reason)
			return
		}
		attempts = []string{d.Model}
		if alt := s.Router.Alternatives(d); len(alt) > 0 {
			attempts = append(attempts, alt[:min(len(alt), s.Cfg.Routing.Retries)]...)
		}
		if _, set := body["reasoning"]; dl.openAI && !set && d.Signals != nil && len(s.Cfg.Routing.ReasoningEffort) > 0 {
			efforts := s.Cfg.Routing.ReasoningEffort
			body["reasoning"] = map[string]any{"effort": efforts[min(d.Signals.Complexity, len(efforts)-1)]}
		}
		w.Header().Set("X-Router-Reason", d.Reason)
		if d.Signals != nil {
			w.Header().Set("X-Router-Topic", d.Signals.Primary)
			w.Header().Set("X-Router-Complexity", strconv.Itoa(d.Signals.Complexity))
			w.Header().Set("X-Router-Risk", strconv.Itoa(d.Signals.Risk))
		}
	}
	if dl.openAI {
		body["usage"] = map[string]any{"include": true} // ask OpenRouter to report cost
	}

	stream := body["stream"] == true
	var failed []string
	for i, m := range attempts {
		body["model"] = m
		payload, _ := json.Marshal(body)
		release := s.Router.Tracker.Acquire(m)
		start := time.Now()
		res, err := s.Upstream(m).Post(r.Context(), dl.path, payload, r.Header)
		retryable := err != nil || res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500
		if retryable && i < len(attempts)-1 {
			status := 0
			if res != nil {
				status = res.StatusCode
				res.Body.Close()
			}
			release()
			slog.Warn("upstream failed, trying next model", "model", m, "status", status, "err", err)
			failed = append(failed, m)
			continue
		}
		if err != nil {
			release()
			dl.error(w, http.StatusBadGateway, err.Error())
			return
		}
		if stream || isSSE(res) {
			s.forward(w, res, dl, m, d, id, failed, stream, time.Since(start))
			release()
			return
		}
		a := readAnswer(res, dl, m, start)
		release()
		s.logChat(id, d, a, failed, false, "")
		writeAnswer(w, s.checkAndEscalate(r, dl, body, req, d, id, a, failed), d, failed)
		return
	}
}

// forward relays a streaming response to the client as it arrives (it cannot be checked: the client
// already has the answer by the time it is complete).
func (s *Server) forward(w http.ResponseWriter, res *http.Response, dl *dialect, model string, d *router.Decision, id string, failed []string, stream bool, latency time.Duration) {
	defer res.Body.Close()
	body := io.Reader(res.Body)
	if res.StatusCode >= 400 && !isSSE(res) && dl.errBody != nil { // a stream refused before it started
		b, _ := io.ReadAll(res.Body)
		body = bytes.NewReader(dl.errBody(res.StatusCode, b))
		res.Header.Set("Content-Type", "application/json")
	}
	setHeaders(w, res.Header, model, d, failed)
	w.WriteHeader(res.StatusCode)
	u := copyAndMeter(w, body, isSSE(res), dl.usage)
	s.logChat(id, d, &answer{api: dl.name, model: model, status: res.StatusCode, usage: u, latency: latency}, failed, stream, "")
	if res.StatusCode >= 400 {
		slog.Warn("upstream error", "model", model, "status", res.StatusCode)
	}
}

// answer is a buffered non-streaming upstream response.
type answer struct {
	api     string // dialect name
	model   string
	status  int
	header  http.Header
	body    []byte
	usage   usage
	latency time.Duration
}

func isSSE(res *http.Response) bool {
	return strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream")
}

func readAnswer(res *http.Response, dl *dialect, model string, start time.Time) *answer {
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 && dl.errBody != nil {
		b = dl.errBody(res.StatusCode, b)
		res.Header.Set("Content-Type", "application/json")
	}
	return &answer{api: dl.name, model: model, status: res.StatusCode, header: res.Header, body: b, usage: dl.usage(b), latency: time.Since(start)}
}

func writeAnswer(w http.ResponseWriter, a *answer, d *router.Decision, failed []string) {
	setHeaders(w, a.header, a.model, d, failed)
	for _, h := range []string{"X-Router-Checked", "X-Router-Escalated"} {
		if v := a.header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(a.status)
	w.Write(a.body)
	if a.status >= 400 {
		slog.Warn("upstream error", "model", a.model, "status", a.status)
	}
}

func setHeaders(w http.ResponseWriter, up http.Header, model string, d *router.Decision, failed []string) {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := up.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if d != nil {
		w.Header().Set("X-Router-Model", model)
		if len(failed) > 0 {
			w.Header().Set("X-Router-Failed", strings.Join(failed, ","))
		}
	}
}

// logChat meters and logs one upstream answer. escalatedFrom is set on the re-sent request of a failed check.
func (s *Server) logChat(id string, d *router.Decision, a *answer, failed []string, stream bool, escalatedFrom string) {
	if a.usage.CostUSD > 0 {
		s.Router.Tracker.AddSpend(a.model, a.usage.CostUSD)
	}
	ev := map[string]any{
		"id": id, "decision": d, "model": a.model, "failed": failed, "status": a.status,
		"cost_usd": a.usage.CostUSD, "prompt_tokens": a.usage.PromptTokens, "completion_tokens": a.usage.CompletionTokens,
		"reasoning_tokens": a.usage.ReasoningTokens, "latency_ms": a.latency.Milliseconds(), "stream": stream,
	}
	if escalatedFrom != "" {
		ev["escalated_from"] = escalatedFrom
	}
	if a.api != "" {
		ev["api"] = a.api
	}
	s.Log.Write("chat", ev)
}

// checkAndEscalate asks the decision model whether a is a good enough answer and, when it is not,
// re-sends the request once to a stronger model. It returns the answer to give the client: a itself
// unless the escalated call succeeded.
func (s *Server) checkAndEscalate(r *http.Request, dl *dialect, body map[string]any, req router.Request, d *router.Decision, id string, a *answer, failed []string) *answer {
	c := s.Cfg.Routing.Check
	if !c.Enabled || d == nil || d.Signals == nil || d.Signals.Complexity < c.MinComplexity || a.status != http.StatusOK ||
		r.Context().Err() != nil { // client gone: nobody to give a better answer to
		return a
	}
	text, ok := dl.text(a.body)
	if !ok {
		return a
	}
	target := s.Router.EscalationTarget(d, a.model, failed)
	if target == "" {
		return a // already the strongest capable model: nothing to escalate to
	}
	pOK, cost, ms, provider, err := s.Router.Check(r.Context(), req, text, d)
	if errors.Is(err, router.ErrCheckSkipped) {
		return a
	}
	if err != nil {
		slog.Warn("answer check failed", "model", a.model, "err", err)
		s.Log.Write("check", map[string]any{"id": id, "model": a.model, "error": err.Error()})
		return a
	}
	checked := strconv.FormatFloat(pOK, 'f', 2, 64)
	a.header.Set("X-Router-Checked", checked)
	passed, final := pOK >= c.Threshold, a
	if !passed {
		if e := s.escalate(r, dl, body, target, id, d, a.model); e != nil {
			e.header.Set("X-Router-Checked", checked)
			e.header.Set("X-Router-Escalated", a.model+"->"+target)
			s.Router.Tracker.SetSticky(req.StickyKey(), target)
			final = e
		}
	}
	escalatedTo := ""
	if final != a {
		escalatedTo = final.model
	}
	s.Log.Write("check", map[string]any{"id": id, "model": a.model, "p_ok": pOK, "passed": passed,
		"escalated_to": escalatedTo, "check_cost_usd": cost, "check_ms": ms, "provider": provider})
	return final
}

// escalate re-sends the request to model, once. It returns nil unless the call succeeded.
func (s *Server) escalate(r *http.Request, dl *dialect, body map[string]any, model, id string, d *router.Decision, from string) *answer {
	body["model"] = model
	payload, _ := json.Marshal(body)
	release := s.Router.Tracker.Acquire(model)
	defer release()
	start := time.Now()
	res, err := s.Upstream(model).Post(r.Context(), dl.path, payload, r.Header)
	if err != nil {
		slog.Warn("escalation failed", "model", model, "err", err)
		return nil
	}
	e := readAnswer(res, dl, model, start)
	s.logChat(id, d, e, nil, false, from)
	if e.status != http.StatusOK {
		slog.Warn("escalation failed", "model", model, "status", e.status)
		return nil
	}
	return e
}

// assistantText returns the first choice's text and whether it is a final answer worth checking:
// non-empty and not a tool-call turn.
func assistantText(b []byte) (string, bool) {
	var v struct {
		Choices []struct {
			Message struct {
				Content   any               `json:"content"`
				ToolCalls []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &v) != nil || len(v.Choices) == 0 || len(v.Choices[0].Message.ToolCalls) > 0 {
		return "", false
	}
	text, _ := content(v.Choices[0].Message.Content)
	return text, strings.TrimSpace(text) != ""
}

// usage is the token/cost accounting extracted from an upstream response.
type usage struct {
	CostUSD          float64
	PromptTokens     int
	CompletionTokens int
	ReasoningTokens  int
}

// merge combines streamed usage split over several events (Anthropic: input tokens in message_start,
// output tokens and cost in message_delta). Every counter is cumulative, and an event may report only
// some of its parts (a message_delta with input_tokens but no cache reads), so the max wins.
func (u usage) merge(n usage) usage {
	return usage{CostUSD: max(u.CostUSD, n.CostUSD), PromptTokens: max(u.PromptTokens, n.PromptTokens),
		CompletionTokens: max(u.CompletionTokens, n.CompletionTokens), ReasoningTokens: max(u.ReasoningTokens, n.ReasoningTokens)}
}

// copyAndMeter streams the upstream body to the client and extracts usage on the way.
func copyAndMeter(w http.ResponseWriter, body io.Reader, stream bool, extract func([]byte) usage) usage {
	if !stream {
		b, _ := io.ReadAll(body)
		w.Write(b)
		return extract(b)
	}
	flusher, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	var u usage
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			if flusher != nil && len(bytes.TrimSpace(line)) == 0 { // end of an SSE event
				flusher.Flush()
			}
			if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok && bytes.Contains(data, []byte(`"usage"`)) {
				u = u.merge(extract(data))
			}
		}
		if err != nil {
			if flusher != nil {
				flusher.Flush()
			}
			return u
		}
	}
}

// extractUsage reads OpenRouter's usage block (cost, tokens, reasoning tokens) from a response body
// or streamed usage chunk.
func extractUsage(b []byte) usage {
	var v struct {
		Usage struct {
			Cost                    float64 `json:"cost"`
			PromptTokens            int     `json:"prompt_tokens"`
			CompletionTokens        int     `json:"completion_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	json.Unmarshal(b, &v)
	return usage{
		CostUSD:          v.Usage.Cost,
		PromptTokens:     v.Usage.PromptTokens,
		CompletionTokens: v.Usage.CompletionTokens,
		ReasoningTokens:  v.Usage.CompletionTokensDetails.ReasoningTokens,
	}
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("anthropic-version") != "" {
		anthropicModels(w, s.Cfg)
		return
	}
	data := []map[string]any{{"id": "auto", "object": "model", "owned_by": "router"}}
	for _, m := range s.Cfg.Models {
		data = append(data, map[string]any{"id": m.ID, "object": "model", "owned_by": "upstream"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// readChat parses an OpenAI chat request, keeping unknown fields intact, and summarises it for routing.
func readChat(r *http.Request) (map[string]any, router.Request, error) {
	var body map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&body); err != nil {
		return nil, router.Request{}, err
	}
	var req router.Request
	var rest strings.Builder // for the privacy pre-check: tool results, earlier turns
	msgs, _ := body["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		text, img := content(m["content"])
		req.Chars += len(text)
		rest.WriteString(text)
		rest.WriteByte('\n')
		req.Vision = req.Vision || img
		switch role {
		case "system", "developer":
			if req.System == "" {
				req.System = text
			}
		case "user":
			req.UserTurns++
			if req.FirstUser == "" {
				req.FirstUser = text
			}
			req.LastUser = text
		}
	}
	req.Rest = rest.String()
	if tools, _ := body["tools"].([]any); len(tools) > 0 {
		req.Tools = true
		tb, _ := json.Marshal(tools)
		req.Chars += len(tb)
	}
	return body, req, nil
}

func content(c any) (string, bool) {
	switch v := c.(type) {
	case string:
		return v, false
	case []any:
		var sb strings.Builder
		img := false
		for _, p := range v {
			part, _ := p.(map[string]any)
			switch part["type"] {
			case "text":
				t, _ := part["text"].(string)
				sb.WriteString(t)
				sb.WriteByte('\n')
			case "image_url", "input_image":
				img = true
			}
		}
		return sb.String(), img
	}
	return "", false
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": map[string]any{"message": msg, "code": code}})
}
