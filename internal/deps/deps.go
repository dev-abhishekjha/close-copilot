//go:build deps

// Package deps pins the initial dependency set so `go mod tidy` keeps it
// before any real code imports these packages. The `deps` build tag keeps
// this file out of every normal build.
//
// Delete an import here once a ticket's code imports the package for real;
// delete this file when it is empty. The comment on each line names the
// ticket that first needs it.
package deps

import (
	_ "github.com/a-h/templ"                                            // CC-1001: server-rendered UI
	_ "github.com/anthropics/anthropic-sdk-go"                          // CC-701: LLM provider
	_ "github.com/google/uuid"                                          // shared specs: finding and run IDs
	_ "github.com/jackc/pgx/v5/pgxpool"                                 // CC-401: store and connection pool
	_ "github.com/modelcontextprotocol/go-sdk/mcp"                      // CC-501, CC-702: MCP servers and clients
	_ "github.com/pgvector/pgvector-go"                                 // CC-804: vector parameters for hybrid search
	_ "github.com/pressly/goose/v3"                                     // CC-401: migrations as a library
	_ "github.com/testcontainers/testcontainers-go"                     // CC-401: integration tests
	_ "github.com/testcontainers/testcontainers-go/modules/postgres"    // CC-401: pgvector/pgvector:pg17 container
	_ "go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"   // CC-904: HTTP spans across services
	_ "go.opentelemetry.io/otel"                                        // CC-904: tracing API
	_ "go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp" // CC-904: export to Langfuse
	_ "go.opentelemetry.io/otel/sdk/trace"                              // CC-904: tracer provider
	_ "go.opentelemetry.io/otel/trace"                                  // CC-904: span types
	_ "golang.org/x/crypto/bcrypt"                                      // CC-1001: password hashes in config/users.yaml
	_ "golang.org/x/time/rate"                                          // CC-1102: per-user rate limits
)
