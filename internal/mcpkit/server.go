package mcpkit

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// NewServer creates a new MCP server instance configured with implementation
// name and version.
func NewServer(name, version string) *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{
		Name:    name,
		Version: version,
	}, nil)
}

// RunStdio connects the server to stdin/stdout and serves requests until
// the context is cancelled or the connection closes.
func RunStdio(ctx context.Context, server *mcp.Server) error {
	if err := server.Run(ctx, &mcp.StdioTransport{}); err != nil {
		return fmt.Errorf("mcpkit: run stdio: %w", err)
	}
	return nil
}
