package mcpkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"regexp"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/store"
)

// maxAuditArgsBytes caps the stored arguments.
const maxAuditArgsBytes = 8 << 10

// AuditSink stores one audit row; *store.Store satisfies it.
type AuditSink interface {
	InsertAuditLog(ctx context.Context, e store.AuditLogEntry) (int64, error)
}

type auditConfig struct {
	server string
	sink   AuditSink
}

var (
	auditMu      sync.RWMutex
	auditServers = map[*mcp.Server]auditConfig{}
)

// EnableAudit makes every tool registered afterwards on s through AddTool
// write one audit row to sink. Call it before registering tools. A failed
// insert fails the call (fail closed).
func EnableAudit(s *mcp.Server, server string, sink AuditSink) {
	auditMu.Lock()
	defer auditMu.Unlock()
	auditServers[s] = auditConfig{server: server, sink: sink}
}

type actorKey struct{}

// ContextWithActor returns ctx carrying the audit actor.
func ContextWithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// ActorFromContext returns the actor set by WithActors, or "unknown".
func ActorFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(actorKey{}).(string); ok && v != "" {
		return v
	}
	return "unknown"
}

var secretKeyRE = regexp.MustCompile(`(?i)token|secret|password|api_?key|authorization`)

// redactJSON replaces values of secret-looking keys at any depth.
func redactJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if secretKeyRE.MatchString(k) {
				x[k] = "[REDACTED]"
			} else {
				x[k] = redactJSON(val)
			}
		}
		return x
	case []any:
		for i := range x {
			x[i] = redactJSON(x[i])
		}
		return x
	default:
		return v
	}
}

// redactArgs redacts raw JSON arguments and truncates the result to 8 KiB.
func redactArgs(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return json.RawMessage(`{"_unparseable":true}`)
	}
	out, err := json.Marshal(redactJSON(v))
	if err != nil {
		return json.RawMessage(`{"_unparseable":true}`)
	}
	if len(out) > maxAuditArgsBytes {
		return json.RawMessage(fmt.Sprintf(`{"_truncated":true,"original_bytes":%d}`, len(out)))
	}
	return out
}

func resultCount(out any) int {
	v := reflect.ValueOf(out)
	if !v.IsValid() {
		return 1
	}
	if k := v.Kind(); k == reflect.Slice || k == reflect.Array {
		return v.Len()
	}
	return 1
}

// AddTool registers h on s like mcp.AddTool. When audit is enabled for s,
// every call writes exactly one audit_log row. All tools must be registered
// through it; a test forbids direct mcp.AddTool calls elsewhere.
func AddTool[In, Out any](s *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	auditMu.RLock()
	cfg, ok := auditServers[s]
	auditMu.RUnlock()
	if !ok {
		mcp.AddTool(s, t, h)
		return
	}
	name := t.Name
	wrapped := func(ctx context.Context, req *mcp.CallToolRequest, in In) (res *mcp.CallToolResult, out Out, err error) {
		start := time.Now()
		count := 0
		defer func() {
			if rec := recover(); rec != nil {
				err = fmt.Errorf("tool %s panicked: %v", name, rec)
				res, out = nil, *new(Out)
				count = 0
			}
			var raw json.RawMessage
			if req != nil && req.Params != nil {
				raw = req.Params.Arguments
			}
			if len(raw) == 0 {
				raw, _ = json.Marshal(in)
			}
			e := store.AuditLogEntry{
				Actor:       ActorFromContext(ctx),
				Server:      &cfg.server,
				Tool:        &name,
				Args:        redactArgs(raw),
				ResultCount: &count,
			}
			lat := int(time.Since(start).Milliseconds())
			e.LatencyMS = &lat
			if err != nil {
				msg := err.Error()
				e.Error = &msg
			} else if res != nil && res.IsError {
				msg := "tool returned an error result"
				e.Error = &msg
			}
			// Detach from cancellation so a dropped client still leaves a row.
			ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if _, ierr := cfg.sink.InsertAuditLog(ictx, e); ierr != nil {
				slog.Error("audit insert failed; failing the call", "server", cfg.server, "tool", name, "error", ierr)
				res, out = nil, *new(Out)
				err = errors.New("audit log unavailable; call refused")
			}
		}()
		res, out, err = h(ctx, req, in)
		if err == nil && (res == nil || !res.IsError) {
			count = resultCount(out)
		}
		return res, out, err
	}
	mcp.AddTool(s, t, wrapped)
}
