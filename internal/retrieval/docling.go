package retrieval

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/httpx"
)

// DoclingTimeout bounds one conversion request.
const DoclingTimeout = 120 * time.Second

const maxErrText = 300

// Docling is a client for docling-serve.
type Docling struct {
	c *caller
}

// NewDocling builds a client for cfg.DoclingURL (empty is an error).
func NewDocling(cfg config.Config) (*Docling, error) {
	c, err := newCaller(cfg, cfg.DoclingURL, config.EnvDoclingURL, httpx.WithTimeout(DoclingTimeout))
	if err != nil {
		return nil, err
	}
	return &Docling{c: c}, nil
}

// ConvertPDF converts the PDF at path to Markdown.
func (d *Docling) ConvertPDF(ctx context.Context, path string) (string, error) {
	const op = "convert pdf"
	data, err := os.ReadFile(path) //nolint:gosec // caller-chosen document path
	if err != nil {
		return "", fmt.Errorf("retrieval: %s: read file: %w", op, err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files"; filename=%q`, filepath.Base(path)))
	h.Set("Content-Type", "application/pdf")
	part, err := mw.CreatePart(h)
	if err != nil {
		return "", fmt.Errorf("retrieval: %s: build request: %w", op, err)
	}
	if _, err := part.Write(data); err != nil {
		return "", fmt.Errorf("retrieval: %s: build request: %w", op, err)
	}
	if err := mw.WriteField("to_formats", "md"); err != nil {
		return "", fmt.Errorf("retrieval: %s: build request: %w", op, err)
	}
	if err := mw.Close(); err != nil {
		return "", fmt.Errorf("retrieval: %s: build request: %w", op, err)
	}

	body, err := d.c.post(ctx, op, "/v1/convert/file", mw.FormDataContentType(), buf.Bytes())
	if err != nil {
		return "", err
	}
	var resp struct {
		Status   string `json:"status"`
		Errors   any    `json:"errors"`
		Document struct {
			MD string `json:"md_content"`
		} `json:"document"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("retrieval: %s: decode response: %w", op, err)
	}
	if strings.EqualFold(resp.Status, "failure") {
		return "", fmt.Errorf("retrieval: %s: docling reported failure: %s", op, truncate(errorsText(resp.Errors), maxErrText))
	}
	if strings.TrimSpace(resp.Document.MD) == "" {
		return "", fmt.Errorf("retrieval: %s: docling returned empty markdown (status %q)", op, resp.Status)
	}
	return resp.Document.MD, nil
}

func errorsText(v any) string {
	switch e := v.(type) {
	case nil:
		return "no error text"
	case string:
		return e
	default:
		b, err := json.Marshal(e)
		if err != nil {
			return "unreadable error text"
		}
		return string(b)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
