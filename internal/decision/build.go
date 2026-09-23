package decision

import (
	"fmt"
	"os"

	"github.com/mmornati/system-one-router/internal/config"
)

// FromConfig builds the provider selector described by the decision section of config.yaml.
func FromConfig(c config.Decision) (*Selector, error) {
	sel := &Selector{Mode: c.Provider, PrivatePolicy: c.Private, Providers: map[string]Provider{}}
	for name, p := range c.Providers {
		key := ""
		if p.APIKeyEnv != "" {
			key = os.Getenv(p.APIKeyEnv)
			if key == "" {
				return nil, fmt.Errorf("decision provider %s: env %s is empty", name, p.APIKeyEnv)
			}
		}
		sel.Providers[name] = NewHTTPProvider(name, p.URL, p.Model, key, p.Local, p.MaxStateChars, p.Timeout)
	}
	if c.Shadow != "" {
		s, ok := sel.Providers[c.Shadow]
		if !ok {
			return nil, fmt.Errorf("shadow provider %q not configured", c.Shadow)
		}
		sel.Shadow = s
	}
	return sel, nil
}
