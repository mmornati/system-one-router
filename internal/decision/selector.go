package decision

import "fmt"

// Selector decides which decision provider answers a given request.
//
//	jev | laya : always that provider
//	auto       : a local provider for private requests (when one is configured), the remote one otherwise
//
// With private=local_only a private request never leaves the machine: it fails if no local provider exists.
type Selector struct {
	Mode          string
	PrivatePolicy string
	Providers     map[string]Provider
	Shadow        Provider // optional; run in background, results only logged
}

func (s *Selector) Pick(private bool) (Provider, error) {
	var local, remote Provider
	for _, p := range s.Providers {
		if p.Local() {
			local = p
		} else {
			remote = p
		}
	}
	if private && s.PrivatePolicy != "ignore" && local != nil && (s.Mode == "auto" || s.PrivatePolicy == "local_only") {
		return local, nil
	}
	if private && s.PrivatePolicy == "local_only" {
		return nil, fmt.Errorf("private request and no local decision provider configured (private: local_only)")
	}
	if s.Mode == "auto" {
		if remote != nil {
			return remote, nil
		}
		if local != nil {
			return local, nil
		}
		return nil, fmt.Errorf("no decision providers configured")
	}
	p, ok := s.Providers[s.Mode]
	if !ok {
		return nil, fmt.Errorf("decision provider %q not configured", s.Mode)
	}
	return p, nil
}
