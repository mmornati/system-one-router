package router

import (
	"sync"
	"time"
)

// Tracker holds live routing state: in-flight requests, daily spend and sticky conversations.
type Tracker struct {
	mu       sync.Mutex
	inflight map[string]int
	spend    map[string]float64
	day      string
	sticky   map[string]stickyEntry
	ttl      time.Duration
	now      func() time.Time
}

type stickyEntry struct {
	model   string
	expires time.Time
}

func NewTracker(ttl time.Duration) *Tracker {
	return &Tracker{inflight: map[string]int{}, spend: map[string]float64{}, sticky: map[string]stickyEntry{}, ttl: ttl, now: time.Now}
}

func (t *Tracker) Acquire(model string) func() {
	t.mu.Lock()
	t.inflight[model]++
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		t.inflight[model]--
		t.mu.Unlock()
	}
}

func (t *Tracker) InFlight(model string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.inflight[model]
}

func (t *Tracker) rollDay() {
	if d := t.now().Format("2006-01-02"); d != t.day {
		t.day, t.spend = d, map[string]float64{}
	}
}

func (t *Tracker) AddSpend(model string, usd float64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollDay()
	t.spend[model] += usd
}

func (t *Tracker) Spent(model string) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rollDay()
	return t.spend[model]
}

func (t *Tracker) Sticky(key string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.sticky[key]
	if !ok || t.now().After(e.expires) {
		delete(t.sticky, key)
		return "", false
	}
	e.expires = t.now().Add(t.ttl)
	t.sticky[key] = e
	return e.model, true
}

func (t *Tracker) SetSticky(key, model string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.sticky[key] = stickyEntry{model: model, expires: t.now().Add(t.ttl)}
}
