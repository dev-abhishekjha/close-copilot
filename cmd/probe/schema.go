package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/config"
)

// schemaDocTypes are the DocTypes the project reads or writes (CC-201).
var schemaDocTypes = []string{
	"Company",
	"Account",
	"Address",
	"Supplier",
	"Customer",
	"Item",
	"Purchase Invoice",
	"Purchase Taxes and Charges",
	"Sales Invoice",
	"Payment Entry",
	"Payment Entry Reference",
	"Journal Entry",
	"Journal Entry Account",
	"GL Entry",
}

// customFieldPageSize is the page size for the Custom Field list.
var customFieldPageSize = 500

// maxCustomFieldPages stops a server that ignores limit_start from looping
// probe forever.
const maxCustomFieldPages = 100

// docSchema is the stable part of one DocType's meta, as written to disk.
type docSchema struct {
	Name          string        `json:"name"`
	Module        string        `json:"module"`
	Istable       int           `json:"istable"`
	IsSubmittable int           `json:"is_submittable"`
	Fields        []fieldSchema `json:"fields"`
}

// fieldSchema is one standard (DocField) or custom (Custom Field) field.
type fieldSchema struct {
	Fieldname string `json:"fieldname"`
	Fieldtype string `json:"fieldtype"`
	Label     string `json:"label"`
	Options   string `json:"options"`
	Reqd      int    `json:"reqd"`
	ReadOnly  int    `json:"read_only"`
	Hidden    int    `json:"hidden"`
	Custom    bool   `json:"custom"`
}

// rawField decodes a DocField or Custom Field row; Frappe sends null or
// omits empty strings, and 0/1 integers for the checks.
type rawField struct {
	Fieldname *string     `json:"fieldname"`
	Fieldtype *string     `json:"fieldtype"`
	Label     *string     `json:"label"`
	Options   *string     `json:"options"`
	Reqd      json.Number `json:"reqd"`
	ReadOnly  json.Number `json:"read_only"`
	Hidden    json.Number `json:"hidden"`
}

func (r rawField) schema(custom bool) (fieldSchema, error) {
	f := fieldSchema{
		Fieldname: deref(r.Fieldname),
		Fieldtype: deref(r.Fieldtype),
		Label:     deref(r.Label),
		Options:   deref(r.Options),
		Custom:    custom,
	}
	var err error
	if f.Reqd, err = flagInt(r.Reqd); err != nil {
		return f, fmt.Errorf("field %q reqd: %w", f.Fieldname, err)
	}
	if f.ReadOnly, err = flagInt(r.ReadOnly); err != nil {
		return f, fmt.Errorf("field %q read_only: %w", f.Fieldname, err)
	}
	if f.Hidden, err = flagInt(r.Hidden); err != nil {
		return f, fmt.Errorf("field %q hidden: %w", f.Fieldname, err)
	}
	return f, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// flagInt reads a Frappe check value (0 or 1, absent means 0).
func flagInt(n json.Number) (int, error) {
	if n == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(n.String())
	if err != nil || (v != 0 && v != 1) {
		return 0, fmt.Errorf("want 0 or 1, got %q", n)
	}
	return v, nil
}

// runSchema implements `probe schema --out <dir>`.
//
// It reads GET /api/resource/DocType/<name> (standard fields) and
// GET /api/resource/Custom Field?filters=[["dt","=",<name>]] (custom
// fields). Accounts User can't read DocType or Custom Field meta, so when
// the bot is refused it switches to the seeder key (ERP_SEED_API_KEY and
// ERP_SEED_API_SECRET) for this subcommand only.
func runSchema(ctx context.Context, cfg config.Config, log *slog.Logger, bot *client, args []string, w io.Writer) error {
	fs := flag.NewFlagSet("probe schema", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	out := fs.String("out", "", "directory to write <kebab-name>.json files to")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("probe schema: %w; %s", err, usage)
	}
	if *out == "" || fs.NArg() > 0 {
		return fmt.Errorf("probe schema: want --out <dir> and nothing else; %s", usage)
	}

	c, err := schemaClient(ctx, cfg, log, bot)
	if err != nil {
		return err
	}

	docs := make([]docSchema, 0, len(schemaDocTypes))
	for _, dt := range schemaDocTypes {
		d, err := fetchSchema(ctx, c, dt)
		if err != nil {
			return fmt.Errorf("probe schema: %s: %w", dt, err)
		}
		docs = append(docs, d)
	}

	if err := os.MkdirAll(*out, 0o750); err != nil {
		return fmt.Errorf("probe schema: %w", err)
	}
	for _, d := range docs {
		data, err := encodeSchema(d)
		if err != nil {
			return fmt.Errorf("probe schema: %s: %w", d.Name, err)
		}
		path := filepath.Join(*out, kebab(d.Name)+".json")
		//nolint:gosec // G306: committed schema docs, not secrets; they must be readable like any other file in the repo.
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return fmt.Errorf("probe schema: %w", err)
		}
		if _, err := fmt.Fprintf(w, "wrote %s (%d fields)\n", path, len(d.Fields)); err != nil {
			return err
		}
	}
	return nil
}

// schemaClient returns the bot client if it may read DocType and Custom
// Field meta, else the seeder client.
func schemaClient(ctx context.Context, cfg config.Config, log *slog.Logger, bot *client) (*client, error) {
	errDocType := probeMeta(ctx, bot, "DocType")
	errCustom := probeMeta(ctx, bot, "Custom Field")
	if errDocType == nil && errCustom == nil {
		return bot, nil
	}
	for _, err := range []error{errDocType, errCustom} {
		if err != nil && !isRefused(err) {
			return nil, fmt.Errorf("probe schema: %w", err)
		}
	}
	if cfg.ERPSeedAPIKey.IsZero() || cfg.ERPSeedAPISecret.IsZero() {
		return nil, fmt.Errorf("probe schema: the bot (Accounts User) is refused on DocType or Custom Field meta and %s/%s are not set",
			config.EnvERPSeedAPIKey, config.EnvERPSeedAPISecret)
	}
	log.Info("the bot (Accounts User) is refused on DocType or Custom Field meta; using the seeder key for schema only")
	return newClient(cfg, cfg.ERPSeedAPIKey, cfg.ERPSeedAPISecret)
}

// probeMeta lists one row of a meta DocType to test read access.
func probeMeta(ctx context.Context, c *client, doctype string) error {
	q := url.Values{}
	q.Set("fields", `["name"]`)
	q.Set("limit_page_length", "1")
	return c.do(ctx, http.MethodGet, resourcePath(doctype), q, nil, nil)
}

func fetchSchema(ctx context.Context, c *client, doctype string) (docSchema, error) {
	var resp struct {
		Data *struct {
			Name          string      `json:"name"`
			Module        string      `json:"module"`
			Istable       json.Number `json:"istable"`
			IsSubmittable json.Number `json:"is_submittable"`
			Fields        []rawField  `json:"fields"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, resourcePath("DocType", doctype), nil, nil, &resp); err != nil {
		return docSchema{}, err
	}
	if resp.Data == nil {
		return docSchema{}, errors.New("the DocType response has no data")
	}
	m := resp.Data
	if m.Name != doctype {
		return docSchema{}, fmt.Errorf("the DocType response is for %q", m.Name)
	}
	d := docSchema{Name: m.Name, Module: m.Module}
	var err error
	if d.Istable, err = flagInt(m.Istable); err != nil {
		return d, fmt.Errorf("istable: %w", err)
	}
	if d.IsSubmittable, err = flagInt(m.IsSubmittable); err != nil {
		return d, fmt.Errorf("is_submittable: %w", err)
	}
	for _, r := range m.Fields {
		f, err := r.schema(false)
		if err != nil {
			return d, err
		}
		d.Fields = append(d.Fields, f)
	}

	custom, err := fetchCustomFields(ctx, c, doctype)
	if err != nil {
		return d, err
	}
	d.Fields = append(d.Fields, custom...)
	slices.SortStableFunc(d.Fields, func(a, b fieldSchema) int {
		return cmp.Or(
			cmp.Compare(a.Fieldname, b.Fieldname),
			compareBool(a.Custom, b.Custom),
			cmp.Compare(a.Fieldtype, b.Fieldtype),
			cmp.Compare(a.Label, b.Label),
			cmp.Compare(a.Options, b.Options),
		)
	})
	if d.Fields == nil {
		d.Fields = []fieldSchema{}
	}
	return d, nil
}

func compareBool(a, b bool) int {
	switch {
	case a == b:
		return 0
	case !a:
		return -1
	default:
		return 1
	}
}

// fetchCustomFields pages through the Custom Field list for doctype.
func fetchCustomFields(ctx context.Context, c *client, doctype string) ([]fieldSchema, error) {
	filters, err := json.Marshal([][]string{{"dt", "=", doctype}})
	if err != nil {
		return nil, err
	}
	var out []fieldSchema
	for page, start := 0, 0; ; page, start = page+1, start+customFieldPageSize {
		if page == maxCustomFieldPages {
			return nil, fmt.Errorf("the Custom Field list did not end after %d pages", maxCustomFieldPages)
		}
		q := url.Values{}
		q.Set("filters", string(filters))
		q.Set("fields", `["fieldname","fieldtype","label","options","reqd","read_only","hidden"]`)
		q.Set("order_by", "fieldname asc")
		q.Set("limit_start", strconv.Itoa(start))
		q.Set("limit_page_length", strconv.Itoa(customFieldPageSize))
		var resp struct {
			Data *[]rawField `json:"data"`
		}
		if err := c.do(ctx, http.MethodGet, resourcePath("Custom Field"), q, nil, &resp); err != nil {
			return nil, err
		}
		if resp.Data == nil {
			return nil, errors.New("the Custom Field response has no data")
		}
		for _, r := range *resp.Data {
			f, err := r.schema(true)
			if err != nil {
				return nil, fmt.Errorf("custom %w", err)
			}
			out = append(out, f)
		}
		if len(*resp.Data) < customFieldPageSize {
			return out, nil
		}
	}
}

// encodeSchema renders d with 2-space indent, no HTML escaping and a
// trailing newline, so two runs give identical bytes.
func encodeSchema(d docSchema) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// kebab turns "Purchase Taxes and Charges" into "purchase-taxes-and-charges".
func kebab(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), "-"))
}
