// Package upstream forwards requests to an OpenAI-compatible provider (OpenRouter by default).
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mmornati/system-one-router/internal/config"
)

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func New(baseURL, apiKey string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: &http.Client{}}
}

// forwarded are the only client headers passed upstream. Client credentials (Authorization, x-api-key)
// never are: the gateway authenticates with its own key.
var forwarded = []string{"Accept", "HTTP-Referer", "X-Title", "anthropic-version", "anthropic-beta"}

// Post sends body to BaseURL+path. The caller owns the response body.
func (c *Client) Post(ctx context.Context, path string, body []byte, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for _, h := range forwarded {
		if v := hdr.Values(h); len(v) > 0 {
			req.Header[http.CanonicalHeaderKey(h)] = append([]string(nil), v...)
		}
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	return c.HTTP.Do(req)
}

type Price struct{ In, Out float64 } // USD per million tokens

// FetchPrices reads the public /models listing (OpenRouter format) and returns prices per model id.
func (c *Client) FetchPrices(ctx context.Context) (map[string]Price, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("models: HTTP %d", res.StatusCode)
	}
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			Pricing struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	prices := map[string]Price{}
	for _, m := range out.Data {
		in, err1 := strconv.ParseFloat(m.Pricing.Prompt, 64)
		o, err2 := strconv.ParseFloat(m.Pricing.Completion, 64)
		if err1 == nil && err2 == nil && in >= 0 && o >= 0 {
			prices[m.ID] = Price{In: in * 1e6, Out: o * 1e6}
		}
	}
	return prices, nil
}

// RefreshPrices overwrites remote model prices in cfg with live ones and returns ids not found upstream.
func (c *Client) RefreshPrices(ctx context.Context, cfg *config.Config) (missing []string, err error) {
	prices, err := c.FetchPrices(ctx)
	if err != nil {
		return nil, err
	}
	for i, m := range cfg.Models {
		if m.Local || m.BaseURL != "" {
			continue
		}
		if p, ok := prices[m.ID]; ok {
			cfg.Models[i].Price = config.Price{In: p.In, Out: p.Out}
		} else {
			missing = append(missing, m.ID)
		}
	}
	return missing, nil
}
