// Command gateway runs the OpenAI-compatible routing gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mmornati/system-one-router/internal/config"
	"github.com/mmornati/system-one-router/internal/decision"
	"github.com/mmornati/system-one-router/internal/gateway"
	"github.com/mmornati/system-one-router/internal/router"
	"github.com/mmornati/system-one-router/internal/store"
	"github.com/mmornati/system-one-router/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "config file")
	envPath := flag.String("env", ".env", "dotenv file (optional)")
	flag.Parse()
	if err := run(*cfgPath, *envPath); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(cfgPath, envPath string) error {
	config.LoadDotEnv(envPath)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	sel, err := decision.FromConfig(cfg.Decision)
	if err != nil {
		return err
	}
	up := upstream.New(cfg.Upstream.BaseURL, os.Getenv(cfg.Upstream.APIKeyEnv))
	if cfg.Upstream.RefreshPrices {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		missing, err := up.RefreshPrices(ctx, cfg)
		cancel()
		if err != nil {
			slog.Warn("price refresh failed, using config prices", "err", err)
		}
		for _, m := range missing {
			slog.Warn("model not found upstream", "model", m)
		}
	}
	log, err := store.Open(cfg.LogPath)
	if err != nil {
		return err
	}
	defer log.Close()

	rt := router.New(cfg, sel)
	rt.OnShadow = func(d *router.Decision, sig *router.Signals, provider string, err error) {
		ev := map[string]any{"id": d.ID, "provider": provider, "primary": d.Signals}
		if err != nil {
			ev["error"] = err.Error()
		} else {
			ev["shadow"] = sig
			ev["agree_topic"] = d.Signals.Primary == sig.Primary
			ev["agree_complexity"] = d.Signals.Complexity == sig.Complexity
		}
		log.Write("shadow", ev)
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: (&gateway.Server{Cfg: cfg, Router: rt, Upstream: upstreams(cfg, up), Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	slog.Info("gateway listening", "addr", cfg.Listen, "decision", cfg.Decision.Provider, "models", len(cfg.Models))
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// upstreams returns the client for a model: its own base_url when set (local runtimes), the default otherwise.
func upstreams(cfg *config.Config, def *upstream.Client) func(string) *upstream.Client {
	byModel := map[string]*upstream.Client{}
	for _, m := range cfg.Models {
		if m.BaseURL == "" {
			continue
		}
		key := os.Getenv(cfg.Upstream.APIKeyEnv)
		if m.APIKeyEnv != "" {
			key = os.Getenv(m.APIKeyEnv)
		} else if m.Local {
			key = ""
		}
		byModel[m.ID] = upstream.New(m.BaseURL, key)
	}
	return func(model string) *upstream.Client {
		if c, ok := byModel[model]; ok {
			return c
		}
		return def
	}
}
