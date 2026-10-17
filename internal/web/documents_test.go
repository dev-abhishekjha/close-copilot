package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/retrieval"
)

// fakeDocs is a DocumentsBackend that records what it was called with.
type fakeDocs struct {
	mu        sync.Mutex
	listCalls []docsListCall
	getCalls  []docsGetCall
	docs      []retrieval.DocSummary
	doc       retrieval.Document
	err       error
}

type docsListCall struct {
	company string
	types   []string
}

type docsGetCall struct{ company, id string }

func (f *fakeDocs) ListDocuments(_ context.Context, company string, types []string) ([]retrieval.DocSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls = append(f.listCalls, docsListCall{company, types})
	return f.docs, f.err
}

func (f *fakeDocs) GetDocument(_ context.Context, company, id string) (retrieval.Document, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls = append(f.getCalls, docsGetCall{company, id})
	return f.doc, f.err
}

func docsServer(t testing.TB, be DocumentsBackend) *testServer {
	t.Helper()
	fs := newFakeRunsStore()
	return newTestServer(t, func(o *Options) {
		o.Runs = &RunsBackend{Store: fs, Execute: func(context.Context, uuid.UUID) error { return nil }}
		o.Documents = be
	})
}

func ctxCookie(company string) *http.Cookie {
	return &http.Cookie{Name: ctxCookieName, Value: url.Values{"company": {company}, "month": {"2026-09"}}.Encode()}
}

func sampleDocs() *fakeDocs {
	acme := "acme"
	return &fakeDocs{
		docs: []retrieval.DocSummary{
			{DocID: "POL-001-accounting-policy", DocType: "policy", Title: "Accounting policy", Sections: 4},
			{DocID: "CT-acme-lease", DocType: "contract", Title: "Office lease", CompanyID: &acme, Sections: 2},
		},
		doc: retrieval.Document{DocID: "CT-acme-lease", DocType: "contract", Title: "Office lease", CompanyID: &acme,
			Sections: []retrieval.DocSection{{Section: "Rent", Content: "first"}, {Section: "", Content: "second"}}},
	}
}

func TestDocumentsListPage(t *testing.T) {
	be := sampleDocs()
	ts := docsServer(t, be)
	rq := ts.as(RoleViewer, http.MethodGet, "/documents", "", "")
	rq.cookies = append(rq.cookies, ctxCookie("globex"))
	w := ts.do(t, rq)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := mainContent(t, w.Body.String())
	for _, want := range []string{`href="/documents/CT-acme-lease"`, "Office lease", "Policy", "Contract", "Shared", ">acme<", ">4<", ">2<", "Documents"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if len(be.listCalls) != 1 || be.listCalls[0].company != "globex" {
		t.Errorf("calls %+v", be.listCalls)
	}
	be.docs = nil
	w = ts.get(t, RoleViewer, "/documents")
	if !strings.Contains(w.Body.String(), "No documents for this company") {
		t.Error("empty state missing")
	}
}

func TestDocumentsTypeFilter(t *testing.T) {
	be := sampleDocs()
	ts := docsServer(t, be)
	w := ts.get(t, RoleViewer, "/documents?type=contract")
	if got := be.listCalls[0].types; !slices.Equal(got, []string{"contract"}) {
		t.Errorf("types %v", got)
	}
	if !strings.Contains(w.Body.String(), `href="/documents?type=contract" aria-current="page"`) {
		t.Errorf("current tab not marked")
	}
	w = ts.get(t, RoleViewer, "/documents?type=%3Cscript%3Ebogus")
	if got := be.listCalls[1].types; len(got) != 0 {
		t.Errorf("unknown type passed %v", got)
	}
	if strings.Contains(w.Body.String(), "bogus") {
		t.Error("unknown type reflected")
	}
}

func TestDocumentsCompanyFromContext(t *testing.T) {
	be := sampleDocs()
	ts := docsServer(t, be)
	rq := ts.as(RoleViewer, http.MethodGet, "/documents?company=globex", "", "")
	rq.cookies = append(rq.cookies, ctxCookie("evil"))
	ts.do(t, rq)
	if got := be.listCalls[0].company; got != "acme" {
		t.Errorf("forged cookie company %q, want default acme", got)
	}
	rq = ts.as(RoleViewer, http.MethodGet, "/documents/CT-acme-lease?company=globex", "", "")
	ts.do(t, rq)
	if got := be.getCalls[0].company; got != "acme" {
		t.Errorf("query company reached the backend: %q", got)
	}
	rq = ts.as(RoleViewer, http.MethodGet, "/documents", "", "")
	rq.body, rq.contentType = "company=globex", "application/x-www-form-urlencoded"
	ts.do(t, rq)
	if got := be.listCalls[1].company; got != "acme" {
		t.Errorf("form company reached the backend: %q", got)
	}
}

func TestDocumentsDocPage(t *testing.T) {
	be := sampleDocs()
	be.doc.Sections = []retrieval.DocSection{
		{Section: "Rent", Content: "<script>alert(1)</script> {{ .X }} **bold** # head"},
		{Section: "", Content: "second"},
	}
	ts := docsServer(t, be)
	w := ts.get(t, RoleChecker, "/documents/CT-acme-lease")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	body := mainContent(t, w.Body.String())
	if strings.Contains(body, "<script>") || strings.Contains(body, "<strong>") {
		t.Error("document text was interpreted")
	}
	for _, want := range []string{"&lt;script&gt;", "{{ .X }}", "**bold**", `<div class="doc-text">`, "Section 2", "Back to documents", "Office lease", "Contract"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Index(body, "Rent") > strings.Index(body, "Section 2") {
		t.Error("sections out of order")
	}
	if len(be.getCalls) != 1 || be.getCalls[0] != (docsGetCall{"acme", "CT-acme-lease"}) {
		t.Errorf("calls %+v", be.getCalls)
	}
}

func TestDocumentsNotFound(t *testing.T) {
	be := &fakeDocs{err: retrieval.ErrDocNotFound}
	ts := docsServer(t, be)
	var pages []string
	for _, id := range []string{"missing-doc", "other-company-doc"} {
		w := ts.get(t, RoleViewer, "/documents/"+id)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Document not found") {
			t.Errorf("%s: %d", id, w.Code)
		}
		pages = append(pages, mainContent(t, w.Body.String()))
	}
	if pages[0] != pages[1] {
		t.Error("missing and other-company pages differ")
	}
	called := len(be.getCalls)
	for _, id := range []string{"..%2Fetc", ".hidden", "-dash", "a%20b", strings.Repeat("a", 101), "a%00b"} {
		w := ts.get(t, RoleViewer, "/documents/"+id)
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Document not found") {
			t.Errorf("%q: %d", id, w.Code)
		}
	}
	if len(be.getCalls) != called {
		t.Error("a malformed id reached the backend")
	}
}

func TestDocumentsBackendError(t *testing.T) {
	const secret = "SECRET-CLAUSE-TEXT"
	be := &fakeDocs{err: errors.New("db exploded: connection refused")}
	be.doc.Sections = []retrieval.DocSection{{Content: secret}}
	ts := docsServer(t, be)
	for _, p := range []string{"/documents", "/documents/CT-acme-lease"} {
		w := ts.get(t, RoleViewer, p)
		if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "Something went wrong") {
			t.Errorf("%s: %d", p, w.Code)
		}
		if strings.Contains(w.Body.String(), "exploded") || strings.Contains(w.Body.String(), secret) {
			t.Errorf("%s: cause in the body", p)
		}
	}
	if !strings.Contains(ts.logs.String(), "CT-acme-lease") {
		t.Error("doc id not logged")
	}
	if strings.Contains(ts.logs.String(), secret) {
		t.Error("document text logged")
	}
}

func TestDocumentsNotConfigured(t *testing.T) {
	ts := newTestServer(t)
	for _, p := range []string{"/documents", "/documents/CT-acme-lease"} {
		w := ts.get(t, RoleViewer, p)
		if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "Documents are not set up on this server") {
			t.Errorf("%s: %d", p, w.Code)
		}
		w = ts.do(t, req{method: http.MethodGet, path: p})
		if w.Code == http.StatusNotImplemented || w.Code == http.StatusOK {
			t.Errorf("anonymous %s: %d", p, w.Code)
		}
	}
}

func TestDocumentsRoles(t *testing.T) {
	be := sampleDocs()
	ts := docsServer(t, be)
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		for _, p := range []string{"/documents", "/documents/CT-acme-lease"} {
			if w := ts.get(t, role, p); w.Code != http.StatusOK {
				t.Errorf("%s %s: %d", role, p, w.Code)
			}
		}
		for _, p := range []string{"/documents", "/documents/CT-acme-lease"} {
			w := ts.do(t, ts.as(role, http.MethodPost, p, "", ""))
			if w.Code != http.StatusMethodNotAllowed && w.Code != http.StatusNotFound {
				t.Errorf("POST %s as %s: %d", p, role, w.Code)
			}
		}
	}
}

func TestDocumentsNoStore(t *testing.T) {
	ts := docsServer(t, sampleDocs())
	for _, p := range []string{"/documents", "/documents/CT-acme-lease", "/documents/missing%20x"} {
		w := ts.get(t, RoleViewer, p)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q", p, got)
		}
		if c := w.Header().Values("Set-Cookie"); len(c) != 0 {
			t.Errorf("%s: Set-Cookie %v", p, c)
		}
	}
}

func TestDocumentsNav(t *testing.T) {
	ts := docsServer(t, sampleDocs())
	for _, role := range []Role{RoleViewer, RoleMaker, RoleChecker} {
		ck, _ := ts.session(role)
		nav := mainNav(t, okBody(t, ts, req{method: http.MethodGet, path: "/", cookies: []*http.Cookie{ck}}))
		if !strings.Contains(nav, `href="/documents"`) {
			t.Errorf("%s: Documents not in the nav", role)
		}
	}
}

func TestDocumentsNoCompany(t *testing.T) {
	be := sampleDocs()
	ts := newTestServer(t, func(o *Options) { o.Documents = be }) // no Runs: no companies
	for _, p := range []string{"/documents", "/documents/CT-acme-lease"} {
		w := ts.get(t, RoleViewer, p)
		if !strings.Contains(w.Body.String(), "No company selected") {
			t.Errorf("%s: %d, no empty state", p, w.Code)
		}
	}
	if len(be.listCalls)+len(be.getCalls) != 0 {
		t.Error("backend called without a company")
	}
}
