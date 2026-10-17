package web

import (
	"context"
	"errors"
	"net/http"
	"regexp"

	"github.com/abhishekjha/close-copilot/internal/retrieval"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// DocumentsBackend is the slice of *retrieval.Lister the documents pages use.
// Both calls filter by company inside the SQL.
type DocumentsBackend interface {
	ListDocuments(ctx context.Context, company string, docTypes []string) ([]retrieval.DocSummary, error)
	GetDocument(ctx context.Context, company, docID string) (retrieval.Document, error)
}

var _ DocumentsBackend = (*retrieval.Lister)(nil)

// docIDPattern is what a {id} must match before the backend is called.
var docIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// docTypes are the filter tabs, in order. Any other ?type is ignored.
var docTypeTabs = []struct{ value, label string }{
	{"", "All"}, {"policy", "Policy"}, {"contract", "Contract"}, {"close_note", "Close note"},
}

func docTypeLabel(t string) string {
	for _, x := range docTypeTabs {
		if x.value != "" && x.value == t {
			return x.label
		}
	}
	return t
}

// documentsRoute answers 501 with a friendly page until Options.Documents is
// set; the role check has run by then.
func (s *Server) documentsRoute(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.documents == nil {
			p := principalFrom(r.Context())
			if p == nil {
				unauthorized(w, r)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			if r.PathValue("id") != "" {
				s.render(w, r, http.StatusNotImplemented, templates.Document(templates.DocumentPage{
					User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf, State: templates.DocsNotConfigured,
				}))
				return
			}
			s.render(w, r, http.StatusNotImplemented, templates.Documents(templates.DocumentsPage{
				User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf, State: templates.DocsNotConfigured,
			}))
			return
		}
		h(w, r)
	})
}

func (s *Server) handleDocumentsPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	page := templates.DocumentsPage{User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf}
	company, _ := s.contextFor(r)
	if company == "" {
		page.State = templates.DocsNoCompany
		s.render(w, r, http.StatusOK, templates.Documents(page))
		return
	}
	cur := ""
	for _, t := range docTypeTabs {
		if t.value != "" && t.value == r.URL.Query().Get("type") {
			cur = t.value
		}
	}
	var types []string
	if cur != "" {
		types = []string{cur}
	}
	docs, err := s.documents.ListDocuments(r.Context(), company, types)
	if err != nil {
		s.log.ErrorContext(r.Context(), "web: documents list", "company", company, "err", err)
		page.State = templates.DocsError
		s.render(w, r, http.StatusInternalServerError, templates.Documents(page))
		return
	}
	for _, t := range docTypeTabs {
		href := "/documents"
		if t.value != "" {
			href += "?type=" + t.value
		}
		page.Tabs.Items = append(page.Tabs.Items, templates.Tab{Label: t.label, Href: href, Current: t.value == cur})
	}
	page.Tabs.Label = "Document type"
	page.Table = templates.TableProps{
		Columns: []templates.Column{
			{Label: "Title"}, {Label: "Document id"}, {Label: "Type"}, {Label: "Scope"}, {Label: "Sections", Align: "right"},
		},
		EmptyTitle: "No documents for this company",
	}
	for _, d := range docs {
		title := d.Title
		if title == "" {
			title = d.DocID
		}
		scope := "Shared"
		if d.CompanyID != nil {
			scope = *d.CompanyID
		}
		page.Table.Rows = append(page.Table.Rows, templates.Row{Cells: []templates.Cell{
			{Text: title, Href: "/documents/" + d.DocID},
			{Text: d.DocID},
			{Text: docTypeLabel(d.DocType)},
			{Text: scope},
			{Text: itoaInt(d.Sections), Num: true},
		}})
	}
	s.render(w, r, http.StatusOK, templates.Documents(page))
}

func (s *Server) handleDocumentPage(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	if p == nil {
		unauthorized(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	page := templates.DocumentPage{User: p.user.Name, Role: string(p.user.Role), CSRFToken: p.csrf}
	id := r.PathValue("id")
	if !docIDPattern.MatchString(id) {
		page.State = templates.DocsNotFound
		s.render(w, r, http.StatusNotFound, templates.Document(page))
		return
	}
	company, _ := s.contextFor(r)
	if company == "" {
		page.State = templates.DocsNoCompany
		s.render(w, r, http.StatusOK, templates.Document(page))
		return
	}
	doc, err := s.documents.GetDocument(r.Context(), company, id)
	switch {
	case errors.Is(err, retrieval.ErrDocNotFound):
		page.State = templates.DocsNotFound
		s.render(w, r, http.StatusNotFound, templates.Document(page))
		return
	case err != nil:
		// The error never carries document text: log the ids only.
		s.log.ErrorContext(r.Context(), "web: documents get", "doc_id", id, "company", company, "err", err)
		page.State = templates.DocsError
		s.render(w, r, http.StatusInternalServerError, templates.Document(page))
		return
	}
	page.Title = doc.Title
	if page.Title == "" {
		page.Title = doc.DocID
	}
	scope := "Shared"
	if doc.CompanyID != nil {
		scope = *doc.CompanyID
	}
	page.Facts = []templates.Fact{
		{Label: "Document id", Value: doc.DocID},
		{Label: "Type", Value: docTypeLabel(doc.DocType)},
		{Label: "Scope", Value: scope},
		{Label: "Sections", Value: itoaInt(len(doc.Sections))},
	}
	for i, sec := range doc.Sections {
		h := sec.Section
		if h == "" {
			h = "Section " + itoaInt(i+1)
		}
		page.Sections = append(page.Sections, templates.DocSectionView{Heading: h, Text: sec.Content})
	}
	s.render(w, r, http.StatusOK, templates.Document(page))
}
