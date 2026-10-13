package web

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/web/templates"
)

// maxEvidenceRows caps the rows shown per evidence reference.
const maxEvidenceRows = 50

// moneyFields are the record fields that hold integer paise.
var moneyFields = map[string]bool{
	"amount": true, "debit": true, "credit": true, "balance": true, "taxable": true,
	"igst": true, "cgst": true, "sgst": true, "grand_total": true,
}

// snapshot is the content of a stored tool_result artifact.
type snapshot struct {
	Server string          `json:"server"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Result any             `json:"result"`
}

// decodeSnapshot parses artifact content with UseNumber so no number is
// ever a float.
func decodeSnapshot(content []byte) (snapshot, error) {
	dec := json.NewDecoder(bytes.NewReader(content))
	dec.UseNumber()
	var s snapshot
	if err := dec.Decode(&s); err != nil {
		return snapshot{}, fmt.Errorf("web: decode snapshot: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return snapshot{}, errors.New("web: decode snapshot: trailing data")
	}
	return s, nil
}

// allRecords lists the records of a tool result: the array itself, the
// array-valued fields of an object in key order, or the value as one record.
func allRecords(result any) []any {
	switch r := result.(type) {
	case []any:
		return r
	case map[string]any:
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var list []any
		for _, k := range keys {
			if arr, ok := r[k].([]any); ok {
				list = append(list, arr...)
			}
		}
		if list == nil {
			return []any{r}
		}
		return list
	case nil:
		return nil
	default:
		return []any{r}
	}
}

// selectRecords is a copy of the verifier's selectRecords
// (internal/agent/explain.go; the web package cannot import it, so the rule
// can drift): a record is kept when one of its top-level string fields
// equals one of ids; with no ids every record is kept.
func selectRecords(result any, ids []string) []any {
	list := allRecords(result)
	if len(ids) == 0 {
		return list
	}
	var out []any
	for _, rec := range list {
		m, ok := rec.(map[string]any)
		if !ok {
			continue
		}
		for _, v := range m {
			if s, ok := v.(string); ok && slices.Contains(ids, s) {
				out = append(out, rec)
				break
			}
		}
	}
	return out
}

// compactJSON renders v as compact JSON text without HTML escaping.
func compactJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "(unprintable)"
	}
	return strings.TrimSpace(b.String())
}

// compactArgs renders raw args as compact JSON text.
func compactArgs(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "{}"
	}
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return "(invalid args)"
	}
	return b.String()
}

// cellText renders one record value: integer money fields through
// Paise.Format, strings as they are, anything else as compact JSON.
func cellText(key string, v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		if moneyFields[key] {
			if n, err := strconv.ParseInt(string(x), 10, 64); err == nil {
				return money.Paise(n).Format()
			}
		}
		return string(x)
	case nil:
		return ""
	default:
		return compactJSON(x)
	}
}

var firstColumns = []string{"name", "txn_id", "voucher_no"}

// erpDoc names the ERPNext document a record of tool stands for: the doctype
// slug and the document name. Either may be empty.
func erpDoc(tool string, rec map[string]any) (slug, name string) {
	str := func(k string) string { s, _ := rec[k].(string); return s }
	switch tool {
	case "list_purchase_invoices":
		return "purchase-invoice", str("name")
	case "list_sales_invoices":
		return "sales-invoice", str("name")
	case "list_payments":
		return "payment-entry", str("name")
	case "list_gl_entries":
		vt := str("voucher_type")
		for _, r := range vt {
			if r != ' ' && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
				return "", str("voucher_no")
			}
		}
		if strings.TrimSpace(vt) == "" {
			return "", str("voucher_no")
		}
		return docSlug(vt), str("voucher_no")
	}
	return "", ""
}

// docSlug lowercases a doctype and turns spaces into hyphens.
func docSlug(doctype string) string {
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(doctype)), " ", "-")
}

// erpURL builds <base>/app/<slug>/<name>. ok is false unless base is an
// http(s) URL with a host and slug and name are not empty.
func erpURL(base, slug, name string) (string, bool) {
	if slug == "" || name == "" {
		return "", false
	}
	u, err := url.Parse(strings.TrimSpace(base))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", false
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.EscapedPath(), "/") + "/app/" + slug + "/" + url.PathEscape(name), true
}

// erpLink is the ERPNext page of a record of tool, if it has one.
func erpLink(base, tool string, rec map[string]any) (string, bool) {
	slug, name := erpDoc(tool, rec)
	return erpURL(base, slug, name)
}

// evidenceBlock builds the panel block of one reference. snap is the loaded
// snapshot; loadErr is set when it could not be read (or the ref has none).
func evidenceBlock(base string, ref store.EvidenceRef, snap *snapshot, loadErr error) templates.EvidenceBlock {
	b := templates.EvidenceBlock{
		Heading: ref.Server + "." + ref.Tool,
		Args:    compactArgs(ref.Args),
		IDs:     ref.IDs,
	}
	switch {
	case ref.Artifact == "":
		b.Note = "snapshot not stored"
		return b
	case loadErr != nil || snap == nil:
		b.Note = "snapshot unavailable"
		return b
	case snap.Server != ref.Server || snap.Tool != ref.Tool:
		b.Note = "snapshot does not match the reference"
		return b
	}
	rows := selectRecords(snap.Result, ref.IDs)
	if len(rows) == 0 {
		if len(ref.IDs) > 0 {
			b.Note = "no matching rows in the snapshot"
		} else {
			b.Note = "the snapshot has no rows"
		}
		return b
	}
	if len(rows) > maxEvidenceRows {
		b.More = len(rows) - maxEvidenceRows
		rows = rows[:maxEvidenceRows]
	}
	maps := make([]map[string]any, len(rows))
	colSet := map[string]bool{}
	for i, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			m = map[string]any{"value": r}
		}
		maps[i] = m
		for k := range m {
			colSet[k] = true
		}
	}
	for _, k := range firstColumns {
		if colSet[k] {
			b.Columns = append(b.Columns, k)
			delete(colSet, k)
		}
	}
	rest := make([]string, 0, len(colSet))
	for k := range colSet {
		rest = append(rest, k)
	}
	sort.Strings(rest)
	b.Columns = append(b.Columns, rest...)
	for _, m := range maps {
		row := templates.EvidenceRow{}
		for _, c := range b.Columns {
			row.Cells = append(row.Cells, cellText(c, m[c]))
		}
		slug, name := erpDoc(ref.Tool, m)
		if name != "" {
			row.Source.Text = name
			if u, ok := erpURL(base, slug, name); ok {
				row.Source.Link = u
			}
			b.HasSource = true
		}
		b.Rows = append(b.Rows, row)
	}
	return b
}
