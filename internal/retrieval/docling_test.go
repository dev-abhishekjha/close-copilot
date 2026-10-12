package retrieval

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

func newDocling(t *testing.T, h http.Handler) *Docling {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	d, err := NewDocling(config.Config{DoclingURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func writePDF(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x.pdf")
	if err := os.WriteFile(p, []byte("%PDF-1.4 fake"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConvertPDFSuccessAndShape(t *testing.T) {
	d := newDocling(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/convert/file" || r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fhs := r.MultipartForm.File["files"]
		if len(fhs) != 1 || fhs[0].Header.Get("Content-Type") != "application/pdf" ||
			r.MultipartForm.Value["to_formats"][0] != "md" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f, _ := fhs[0].Open()
		b, _ := io.ReadAll(f)
		if string(b) != "%PDF-1.4 fake" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","document":{"md_content":"# Title\n\nbody"}}`))
	}))
	md, err := d.ConvertPDF(t.Context(), writePDF(t))
	if err != nil || md != "# Title\n\nbody" {
		t.Fatalf("md=%q err=%v", md, err)
	}
}

func TestConvertPDFFailure(t *testing.T) {
	d := newDocling(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"failure","errors":[{"error_message":"corrupt pdf"}],"document":{"md_content":"x"}}`))
	}))
	_, err := d.ConvertPDF(t.Context(), writePDF(t))
	if err == nil || !strings.Contains(err.Error(), "corrupt pdf") {
		t.Fatalf("err = %v", err)
	}
}

func TestConvertPDFEmptyMarkdown(t *testing.T) {
	d := newDocling(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","document":{"md_content":"  "}}`))
	}))
	if _, err := d.ConvertPDF(t.Context(), writePDF(t)); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestConvertPDFMissingFile(t *testing.T) {
	var calls atomic.Int32
	d := newDocling(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	if _, err := d.ConvertPDF(t.Context(), filepath.Join(t.TempDir(), "nope.pdf")); err == nil || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestConvertPDFRetry(t *testing.T) {
	var calls atomic.Int32
	d := newDocling(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"status":"success","document":{"md_content":"ok"}}`))
	}))
	d.c.sleep = func(context.Context, time.Duration) error { return nil }
	md, err := d.ConvertPDF(t.Context(), writePDF(t))
	if err != nil || md != "ok" || calls.Load() != 2 {
		t.Fatalf("md=%q err=%v calls=%d", md, err, calls.Load())
	}
}

func TestNewDoclingEmptyURL(t *testing.T) {
	if _, err := NewDocling(config.Config{}); err == nil {
		t.Fatal("want error")
	}
}
