package retrieval

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// listRows is a pgx.Rows over fixed rows; Scan copies into the pointers.
type listRows struct {
	pgx.Rows
	data [][]any
	i    int
}

func (r *listRows) Next() bool { r.i++; return r.i <= len(r.data) }
func (r *listRows) Close()     {}
func (r *listRows) Err() error { return nil }
func (r *listRows) Scan(dest ...any) error {
	for i, v := range r.data[r.i-1] {
		switch d := dest[i].(type) {
		case *string:
			*d = v.(string)
		case **string:
			if v == nil {
				*d = nil
			} else {
				s := v.(string)
				*d = &s
			}
		case *int:
			*d = v.(int)
		}
	}
	return nil
}

type listQuerier struct {
	data  [][]any
	sqls  []string
	args  [][]any
	calls int
}

func (q *listQuerier) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	q.calls++
	q.sqls = append(q.sqls, sql)
	q.args = append(q.args, args)
	return &listRows{data: q.data}, nil
}

const pred = "company_id IS NULL OR company_id = $1"

func TestListDocumentsSQLFiltersCompany(t *testing.T) {
	q := &listQuerier{}
	l := NewLister(q)
	ctx := context.Background()
	if _, err := l.ListDocuments(ctx, "sharma", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := l.ListDocuments(ctx, "sharma", []string{"contract"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.GetDocument(ctx, "sharma", "doc-1"); !errors.Is(err, ErrDocNotFound) {
		t.Fatal(err)
	}
	for i, sql := range q.sqls {
		w := strings.Index(sql, "WHERE")
		p := strings.Index(sql, pred)
		if w < 0 || p < w {
			t.Errorf("stmt %d: predicate not in WHERE: %s", i, sql)
		}
		for _, kw := range []string{"\nGROUP BY", "\nORDER BY", "\nLIMIT"} {
			if k := strings.Index(sql, kw); k >= 0 && k < p {
				t.Errorf("stmt %d: %s before the predicate", i, kw)
			}
		}
		if q.args[i][0] != "sharma" {
			t.Errorf("stmt %d: $1 = %v", i, q.args[i][0])
		}
	}
}

func TestListDocumentsTypes(t *testing.T) {
	tests := []struct {
		name  string
		types []string
		want  bool
	}{
		{"none", nil, false},
		{"one", []string{"contract"}, true},
		{"two", []string{"policy", "contract"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := &listQuerier{}
			if _, err := NewLister(q).ListDocuments(context.Background(), "c", tc.types); err != nil {
				t.Fatal(err)
			}
			sql := q.sqls[0]
			if has := strings.Contains(sql, "doc_type = ANY($2)"); has != tc.want {
				t.Errorf("type predicate = %v, want %v: %s", has, tc.want, sql)
			}
			if tc.want && strings.Index(sql, "doc_type = ANY($2)") > strings.Index(sql, "GROUP BY") {
				t.Error("type predicate is not in the WHERE")
			}
			if got := len(q.args[0]); got != 1+btoi(tc.want) {
				t.Errorf("%d args", got)
			}
		})
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

func TestListDocumentsEmptyCompany(t *testing.T) {
	q := &listQuerier{}
	l := NewLister(q)
	if _, err := l.ListDocuments(context.Background(), "", nil); !errors.Is(err, ErrNoCompany) {
		t.Errorf("ListDocuments: %v", err)
	}
	if _, err := l.GetDocument(context.Background(), "", "x"); !errors.Is(err, ErrNoCompany) {
		t.Errorf("GetDocument: %v", err)
	}
	if q.calls != 0 {
		t.Errorf("%d queries issued", q.calls)
	}
}

func TestListDocumentsRows(t *testing.T) {
	q := &listQuerier{data: [][]any{
		{"c1", "contract", "sharma", "Lease", 3},
		{"p1", "policy", nil, nil, 1},
	}}
	got, err := NewLister(q).ListDocuments(context.Background(), "sharma", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "Lease" || got[0].Sections != 3 || *got[0].CompanyID != "sharma" ||
		got[1].CompanyID != nil || got[1].Title != "" {
		t.Errorf("got %+v", got)
	}
}

func TestGetDocumentOrderAndNulls(t *testing.T) {
	q := &listQuerier{data: [][]any{
		{"contract", "sharma", nil, "Lease", "first"},
		{"contract", "sharma", "§2", nil, "second"},
	}}
	d, err := NewLister(q).GetDocument(context.Background(), "sharma", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Lease" || d.DocType != "contract" || d.DocID != "c1" {
		t.Errorf("doc %+v", d)
	}
	if len(d.Sections) != 2 || d.Sections[0] != (DocSection{"", "first"}) || d.Sections[1] != (DocSection{"§2", "second"}) {
		t.Errorf("sections %+v", d.Sections)
	}
	if !strings.Contains(q.sqls[0], "ORDER BY id") {
		t.Error("not ordered by id")
	}
	if q.args[0][1] != "c1" {
		t.Errorf("args %v", q.args[0])
	}
}

func TestGetDocumentNotFound(t *testing.T) {
	_, err := NewLister(&listQuerier{}).GetDocument(context.Background(), "sharma", "nope")
	if !errors.Is(err, ErrDocNotFound) {
		t.Errorf("err %v", err)
	}
}

type errQuerier struct{ err error }

func (q errQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, q.err }

func TestListDocumentsQueryError(t *testing.T) {
	boom := errors.New("boom")
	l := NewLister(errQuerier{boom})
	if _, err := l.ListDocuments(context.Background(), "c", nil); !errors.Is(err, boom) {
		t.Errorf("ListDocuments: %v", err)
	}
	if _, err := l.GetDocument(context.Background(), "c", "d"); !errors.Is(err, boom) {
		t.Errorf("GetDocument: %v", err)
	}
}
