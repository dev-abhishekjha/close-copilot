// Command mcp-evidence is the Evidence MCP server: bank lines, GSTR-2B entries and document search from Postgres.
//
// Built in CC-501, CC-503, CC-805.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/retrieval"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
)

func main() {
	cli.Main("mcp-evidence", []string{
		config.EnvDatabaseURL,
		config.EnvMCPTokenAgent,
		config.EnvTEIEmbedURL,
		config.EnvTEIRerankURL,
	}, run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("mcp-evidence", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	transport := fs.String("transport", "http", "transport: http (scoped: needs MCP_SCOPE_KEY, and every /mcp request a valid X-Run-Scope) or stdio (unscoped: a local process with no network)")
	addr := fs.String("addr", ":7002", "listen address for HTTP transport")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("mcp-evidence: parse flags: %w", err)
	}

	if *transport != "http" && *transport != "stdio" {
		return fmt.Errorf("mcp-evidence: unknown transport %q (want http or stdio)", *transport)
	}
	if *transport == "http" && cfg.MCPScopeKey.IsZero() {
		return errNoScopeKey
	}

	stopTrace, err := telemetry.Init(ctx, "mcp-evidence", cfg, log)
	if err != nil {
		return err
	}
	defer stopTrace()

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return fmt.Errorf("mcp-evidence: connect to database: %w", err)
	}
	defer st.Close()

	// Built once, before serving: a server that cannot answer search_documents
	// should not start.
	tei, err := retrieval.NewTEI(cfg)
	if err != nil {
		return fmt.Errorf("mcp-evidence: %w", err)
	}
	searcher := retrieval.NewSearcher(st.Pool(), tei, tei)

	switch *transport {
	case "stdio":
		// Unscoped: a local process with no network, driven by one user.
		log.Info("starting mcp-evidence in stdio mode")
		return mcpkit.RunStdio(ctx, newEvidenceServer(st, nil, searcher, st))
	case "http":
		routes, opts, err := httpRoutes(cfg, st, searcher, st)
		if err != nil {
			return err
		}
		return mcpkit.ServeHTTP(ctx, *addr, routes, log, opts...)
	default:
		return fmt.Errorf("mcp-evidence: unknown transport %q (want http or stdio)", *transport)
	}
}

// errNoScopeKey refuses an HTTP server without MCP_SCOPE_KEY: /mcp would
// otherwise read any company and any month.
var errNoScopeKey = errors.New("mcp-evidence: --transport http needs " + config.EnvMCPScopeKey +
	" (see .env.example); without it /mcp can't bind calls to a run")

// newEvidenceServer builds the evidence server over st, audited to each
// sink (the binary passes the store; unit tests pass none). scope binds
// calls to their run (nil only for stdio and tests).
func newEvidenceServer(st *store.Store, scope *mcpkit.Scoper, searcher evidence.DocSearcher, sinks ...mcpkit.AuditSink) *mcp.Server {
	s := mcpkit.NewServer("close-copilot-evidence", buildinfo.Version)
	for _, sink := range sinks {
		mcpkit.EnableAudit(s, "evidence", sink)
	}
	evidence.RegisterTools(s, st, evidence.WithScope(scope), evidence.WithSearcher(searcher))
	return s
}

// httpRoutes is what --transport http serves: the scoped evidence tools at
// /mcp. The same Scoper guards /mcp at the HTTP layer (401 without a valid
// X-Run-Scope) and in every tool handler (out_of_scope).
func httpRoutes(cfg config.Config, st *store.Store, searcher evidence.DocSearcher, sinks ...mcpkit.AuditSink) (map[string]mcpkit.Route, []mcpkit.Option, error) {
	if cfg.MCPScopeKey.IsZero() {
		return nil, nil, errNoScopeKey
	}
	scoper, err := mcpkit.NewScoper(cfg.MCPScopeKey.Reveal())
	if err != nil {
		return nil, nil, fmt.Errorf("mcp-evidence: %w", err)
	}
	routes := map[string]mcpkit.Route{
		"/mcp": mcpkit.NewAgentRoute(newEvidenceServer(st, scoper, searcher, sinks...), cfg),
	}
	opts := []mcpkit.Option{
		mcpkit.WithActors(map[string]string{"/mcp": "agent"}),
		mcpkit.WithScoper(scoper, "/mcp"),
	}
	return routes, opts, nil
}
