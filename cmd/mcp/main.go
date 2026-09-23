// Command mcp exposes the gateway's routing decision and chat completion as MCP tools
// (route, delegate, feedback), so agents like Claude Code can call it directly.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	gatewayURL := flag.String("gateway", envOr("ROUTER_URL", "http://127.0.0.1:8787"), "base URL of the running gateway")
	httpAddr := flag.String("http", "", "serve streamable HTTP on this address instead of stdio (e.g. :8790)")
	flag.Parse()

	c := &client{baseURL: *gatewayURL, hc: &http.Client{Timeout: 5 * time.Minute}}
	srv := newServer(c)

	if *httpAddr == "" {
		if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			slog.Error("fatal", "err", err)
			os.Exit(1)
		}
		return
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
	slog.Info("mcp server listening", "addr", *httpAddr, "gateway", *gatewayURL)
	if err := http.ListenAndServe(*httpAddr, handler); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// newServer builds the MCP server with the route/delegate/feedback tools wired to c.
func newServer(c *client) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "system-one-router", Version: "0.1.0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "route",
		Description: "Dry-run the router's decision for a prompt: which model it would pick and why, " +
			"without calling any model. Free (no LLM billed besides the small decision call the gateway makes internally). " +
			"Use it to check routing before delegating, or just to inspect the decision.",
	}, c.route)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "delegate",
		Description: "Offload a self-contained subtask (a summary, boilerplate, docs, a simple code change) to a " +
			"model chosen by the router, usually cheaper than the calling agent. Returns the answer as text. " +
			"Not for tasks needing this conversation's full context.",
	}, c.delegate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "feedback",
		Description: "Report whether a delegated answer (from route or delegate, by its request_id) was good or bad, with an optional comment. Feeds the router's skill re-fitting.",
	}, c.feedback)

	return srv
}
