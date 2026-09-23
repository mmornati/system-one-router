// Package gateway exposes an OpenAI-compatible API. Requests with model "auto" are routed;
// any other model name is passed through untouched, so clients can point at the gateway blindly.
package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/router"
	"github.com/mmornati/system-one-router/internal/store"
	"github.com/mmornati/system-one-router/internal/upstream"
)

const maxBody = 32 << 20

type Server struct {
	Cfg      *config.Config
	Router   *router.Router
	Upstream func(model string) *upstream.Client
	Log      *store.Log
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.chat)
	mux.HandleFunc("POST /route", s.route)
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return mux
}

func isAuto(model string) bool { return model == "auto" || model == "router/auto" || model == "" }

// route returns the routing decision for a chat request without calling any model (debugging / dry runs).
func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	_, req, err := readChat(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	d := s.Router.Route(r.Context(), req)
	s.Log.Write("route_dry", d)
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	body, req, err := readChat(r)
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	model, _ := body["model"].(string)
	attempts := []string{model}
	var d *router.Decision
	if isAuto(model) {
		d = s.Router.Route(r.Context(), req)
		if d.Refused {
			s.Log.Write("refused", d)
			httpError(w, http.StatusUnprocessableEntity, "request refused by routing policy: "+d.Reason)
			return
		}
		attempts = []string{d.Model}
		if alt := s.Router.Alternatives(d); len(alt) > 0 {
			attempts = append(attempts, alt[:min(len(alt), s.Cfg.Routing.Retries)]...)
		}
		if _, set := body["reasoning"]; !set && d.Signals != nil && len(s.Cfg.Routing.ReasoningEffort) > 0 {
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
	body["usage"] = map[string]any{"include": true} // ask OpenRouter to report cost

	var failed []string
	for i, m := range attempts {
		body["model"] = m
		payload, _ := json.Marshal(body)
		release := s.Router.Tracker.Acquire(m)
		res, err := s.Upstream(m).Post(r.Context(), "/chat/completions", payload, r.Header)
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
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		s.forward(w, res, m, d, failed, body["stream"] == true)
		release()
		return
	}
}

func (s *Server) forward(w http.ResponseWriter, res *http.Response, model string, d *router.Decision, failed []string, stream bool) {
	defer res.Body.Close()
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if d != nil {
		w.Header().Set("X-Router-Model", model)
		if len(failed) > 0 {
			w.Header().Set("X-Router-Failed", strings.Join(failed, ","))
		}
	}
	w.WriteHeader(res.StatusCode)
	cost := copyAndMeter(w, res.Body, strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream"))
	if cost > 0 {
		s.Router.Tracker.AddSpend(model, cost)
	}
	s.Log.Write("chat", map[string]any{"decision": d, "model": model, "failed": failed, "status": res.StatusCode, "cost_usd": cost, "stream": stream})
	if res.StatusCode >= 400 {
		slog.Warn("upstream error", "model", model, "status", res.StatusCode)
	}
}

// copyAndMeter streams the upstream body to the client and extracts usage.cost on the way.
func copyAndMeter(w http.ResponseWriter, body io.Reader, stream bool) float64 {
	if !stream {
		b, _ := io.ReadAll(body)
		w.Write(b)
		return usageCost(b)
	}
	flusher, _ := w.(http.Flusher)
	br := bufio.NewReaderSize(body, 64<<10)
	var cost float64
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			w.Write(line)
			if flusher != nil && len(bytes.TrimSpace(line)) == 0 { // end of an SSE event
				flusher.Flush()
			}
			if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok && bytes.Contains(data, []byte(`"usage"`)) {
				if c := usageCost(data); c > 0 {
					cost = c
				}
			}
		}
		if err != nil {
			if flusher != nil {
				flusher.Flush()
			}
			return cost
		}
	}
}

func usageCost(b []byte) float64 {
	var v struct {
		Usage struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
	}
	json.Unmarshal(b, &v)
	return v.Usage.Cost
}

func (s *Server) models(w http.ResponseWriter, _ *http.Request) {
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
	msgs, _ := body["messages"].([]any)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		role, _ := m["role"].(string)
		text, img := content(m["content"])
		req.Chars += len(text)
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
