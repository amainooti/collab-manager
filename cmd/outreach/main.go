// Command outreach runs the outreach engine either as an HTTP server (REST
// API + kanban web UI) or as an MCP server over stdio. Both modes share the
// same core.Service, backed by the same SQLite database.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"outreach/internal/anthropicx"
	"outreach/internal/api"
	"outreach/internal/core"
	"outreach/internal/hfx"
	"outreach/internal/llmrouter"
	"outreach/internal/mcpserver"
	"outreach/internal/store"
	"outreach/internal/web"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	dbPath := envOr("OUTREACH_DB", "outreach.db")
	db, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer db.Close()

	svc := core.New(db, buildLLM())

	switch os.Args[1] {
	case "serve":
		runServe(svc)
	case "mcp":
		runMCP(svc)
	default:
		usage()
		os.Exit(1)
	}
}

// buildLLM wires Claude as primary and, if HF_TOKEN is set, a Hugging Face
// model as fallback — Claude is tried first on every call; only on failure
// (rate limited, quota exhausted, outage, or simply no ANTHROPIC_API_KEY)
// does it fall through to the free model. Skipping the router entirely when
// only one provider is configured keeps that common case (one key set) from
// paying for an interface hop it doesn't need — not that it'd be
// measurable, just no reason to add it.
func buildLLM() core.LLM {
	var primary core.LLM
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		primary = anthropicx.New()
	} else {
		log.Println("ANTHROPIC_API_KEY not set — Claude disabled for this run")
	}

	hf := hfx.New()
	var fallback core.LLM
	if hf.Configured() {
		fallback = hf
	}

	switch {
	case primary != nil && fallback != nil:
		log.Printf("Hugging Face fallback enabled (%s) — used if Claude errors or isn't reachable", hf.ModelID())
		return llmrouter.New(primary, fallback)
	case primary != nil:
		return primary
	case fallback != nil:
		log.Printf("no ANTHROPIC_API_KEY set — running on Hugging Face (%s) as the sole provider", hf.ModelID())
		return fallback
	default:
		log.Fatal("no model provider configured — set ANTHROPIC_API_KEY and/or HF_TOKEN")
		return nil
	}
}

func runServe(svc *core.Service) {
	addr := envOr("OUTREACH_ADDR", ":8090")

	mux := http.NewServeMux()
	api.New(svc).Register(mux)
	web.New(svc).Register(mux)

	log.Printf("outreach serving on %s (db=%s)", addr, envOr("OUTREACH_DB", "outreach.db"))
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("serve: %v", err)
	}
}

func runMCP(svc *core.Service) {
	server := mcpserver.NewServer(svc)
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatalf("mcp server: %v", err)
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: outreach <serve|mcp>")
	fmt.Fprintln(os.Stderr, "  serve  run the REST API + web UI (default addr :8090, override with OUTREACH_ADDR)")
	fmt.Fprintln(os.Stderr, "  mcp    run an MCP server over stdio")
	fmt.Fprintln(os.Stderr, "env: ANTHROPIC_API_KEY and/or HF_TOKEN (at least one required), OUTREACH_DB (default outreach.db)")
	fmt.Fprintln(os.Stderr, "     HF_MODEL, HF_BASE_URL to customize the Hugging Face fallback")
}
