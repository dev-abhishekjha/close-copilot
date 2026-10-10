package books

import (
	"context"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/frappe"
)

// Bounds on one tool call.
const (
	// ToolTimeout bounds one whole tool call: every ERPNext request it
	// makes shares this one deadline.
	ToolTimeout = 90 * time.Second
	// MaxDocsPerCall is the most documents list_purchase_invoices,
	// list_sales_invoices, list_payments and list_recurring_suppliers read
	// in one call. Over it the call fails before any document is fetched.
	MaxDocsPerCall = 1000
	// MaxTextRunes is the longest free-text output field; longer text is
	// cut and ends with TruncatedMarker.
	MaxTextRunes = 500
	// TruncatedMarker ends a free-text field cut to MaxTextRunes.
	TruncatedMarker = " …[truncated]"
)

// toolTimeout is ToolTimeout, read when RegisterTools runs; tests shrink it.
var toolTimeout = ToolTimeout

// The fixed messages the model sees when a call fails for any reason other
// than bad input. The full error goes to the server log only, so no ERPNext
// response text, base URL or query string reaches the model.
const (
	MsgPermission  = "permission denied by ERPNext"
	MsgNotFound    = "not found"
	MsgUnbalanced  = "the ledger does not balance for this period; ERPNext data needs checking"
	MsgUnavailable = "ERPNext unavailable, try again later"
)

// tool adapts a handler to the SDK: it gives the whole call one deadline
// and turns any error into a model-safe one.
func tool[In, Out any](h *handlers, fn func(context.Context, In) (Out, error)) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		ctx, cancel := context.WithTimeout(ctx, h.timeout)
		defer cancel()
		out, err := fn(ctx, in)
		if err != nil {
			var zero Out
			name := ""
			if req != nil && req.Params != nil {
				name = req.Params.Name
			}
			return nil, zero, h.toModelError(ctx, name, err)
		}
		return nil, out, nil
	}
}

// toModelError maps err to what the model may see. Bad input keeps its
// field-named message; everything else becomes one of the fixed messages
// by class, and the full error is logged.
func (h *handlers) toModelError(ctx context.Context, toolName string, err error) error {
	var ie *inputError
	if errors.As(err, &ie) {
		h.logger().InfoContext(ctx, "books tool: invalid input", "tool", toolName, "err", ie.Error())
		return ie
	}
	h.logger().ErrorContext(ctx, "books tool failed", "tool", toolName, "err", err.Error())
	switch {
	case frappe.IsPermission(err):
		return errors.New(MsgPermission)
	case frappe.IsNotFound(err):
		return errors.New(MsgNotFound)
	case errors.Is(err, ErrUnbalanced):
		return errors.New(MsgUnbalanced)
	default:
		return errors.New(MsgUnavailable)
	}
}

func (h *handlers) logger() *slog.Logger {
	if h.deps.Logger != nil {
		return h.deps.Logger
	}
	return slog.Default()
}

// clipText cuts s to MaxTextRunes runes plus TruncatedMarker. Free text
// from ERPNext (remarks, descriptions, references) is otherwise unbounded.
func clipText(s string) string {
	if utf8.RuneCountInString(s) <= MaxTextRunes {
		return s
	}
	r := []rune(s)
	return string(r[:MaxTextRunes]) + TruncatedMarker
}
