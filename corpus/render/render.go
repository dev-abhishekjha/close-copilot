// Package render turns the corpus Markdown subset into byte-stable PDFs with
// a pure-Go renderer (no network, no TeX). The PDFs carry real text,
// headings and tables, so a document parser can read them.
//
// Supported Markdown: "#", "##" and "###" headings, paragraphs, "- "
// bullets, pipe tables, **bold** and single-line HTML comments (skipped).
// The core Helvetica fonts have no rupee sign, so the PDF prints it as "Rs.".
package render

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codeberg.org/go-pdf/fpdf"
)

var created = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// PDF renders the Markdown source to PDF bytes. The title goes in the
// document properties.
func PDF(title, md string) ([]byte, error) {
	p := fpdf.New("P", "mm", "A4", "")
	p.SetCreationDate(created)
	p.SetModificationDate(created)
	p.SetCompression(false)
	p.SetCatalogSort(true)
	p.SetTitle(title, false)
	p.SetAuthor("Close Copilot synthetic corpus", false)
	p.SetCreator("close-copilot corpus generator", false)
	p.SetMargins(20, 20, 20)
	p.SetAutoPageBreak(true, 20)
	tr := p.UnicodeTranslatorFromDescriptor("")
	p.SetFooterFunc(func() {
		p.SetY(-14)
		p.SetFont("Helvetica", "", 8)
		p.CellFormat(0, 6, fmt.Sprintf("%s - page %d", tr(clean(title)), p.PageNo()), "", 0, "C", false, 0, "")
	})
	p.AddPage()

	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var para []string
	flush := func() {
		if len(para) > 0 {
			p.SetFont("Helvetica", "", 11)
			rich(p, tr, strings.Join(para, " "), 6.3, 0)
			p.Ln(7)
			para = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		l := strings.TrimRight(lines[i], " \t")
		t := strings.TrimSpace(l)
		switch {
		case t == "":
			flush()
		case strings.HasPrefix(t, "<!--") && strings.HasSuffix(t, "-->"):
			// machine-readable marker: not rendered
		case strings.HasPrefix(t, "### "):
			flush()
			heading(p, tr, t[4:], 11.5, 3)
		case strings.HasPrefix(t, "## "):
			flush()
			heading(p, tr, t[3:], 14, 5)
		case strings.HasPrefix(t, "# "):
			flush()
			heading(p, tr, t[2:], 19, 6)
		case strings.HasPrefix(t, "- "):
			flush()
			p.SetFont("Helvetica", "", 11)
			p.SetX(24)
			p.CellFormat(4, 6.3, "-", "", 0, "L", false, 0, "")
			rich(p, tr, t[2:], 6.3, 28)
			p.Ln(7.0)
		case strings.HasPrefix(t, "|"):
			flush()
			var rows [][]string
			for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
				cells := splitRow(lines[i])
				if isSeparator(cells) {
					continue
				}
				rows = append(rows, cells)
			}
			i--
			table(p, tr, rows)
			p.Ln(5)
		default:
			para = append(para, t)
		}
	}
	flush()
	var buf bytes.Buffer
	if err := p.Output(&buf); err != nil {
		return nil, fmt.Errorf("render %s: %w", title, err)
	}
	return buf.Bytes(), nil
}

// Pages returns the page count of a PDF made by PDF.
func Pages(pdf []byte) int {
	return bytes.Count(pdf, []byte("/Type /Page\n"))
}

func clean(s string) string {
	s = strings.ReplaceAll(s, "₹", "Rs. ")
	s = strings.ReplaceAll(s, "Rs. ", "Rs.")
	s = strings.ReplaceAll(s, "≤", "<=")
	s = strings.ReplaceAll(s, "≥", ">=")
	return s
}

func heading(p *fpdf.Fpdf, tr func(string) string, text string, size, gap float64) {
	if p.GetY() > 250 {
		p.AddPage()
	}
	p.Ln(gap)
	p.SetFont("Helvetica", "B", size)
	p.MultiCell(0, size*0.55, tr(clean(strings.ReplaceAll(text, "**", ""))), "", "L", false)
	p.Ln(2)
}

// rich writes text with **bold** toggling, wrapping within the margins.
// indent, when non-zero, is the left x of continuation lines.
func rich(p *fpdf.Fpdf, tr func(string) string, text string, lh, indent float64) {
	if indent > 0 {
		p.SetLeftMargin(indent)
		defer p.SetLeftMargin(20)
		p.SetX(indent)
	}
	parts := strings.Split(clean(text), "**")
	for i, part := range parts {
		style := ""
		if i%2 == 1 {
			style = "B"
		}
		p.SetFont("Helvetica", style, 11)
		p.Write(lh, tr(part))
	}
}

func splitRow(l string) []string {
	l = strings.TrimSpace(l)
	l = strings.TrimPrefix(l, "|")
	l = strings.TrimSuffix(l, "|")
	parts := strings.Split(l, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func isSeparator(cells []string) bool {
	for _, c := range cells {
		if strings.Trim(c, "-: ") != "" {
			return false
		}
	}
	return len(cells) > 0
}

func table(p *fpdf.Fpdf, tr func(string) string, rows [][]string) {
	if len(rows) == 0 {
		return
	}
	n := len(rows[0])
	total := 0
	weights := make([]int, n)
	for _, r := range rows {
		for c := 0; c < n && c < len(r); c++ {
			if w := min(len(r[c]), 40) + 6; w > weights[c] {
				weights[c] = w
			}
		}
	}
	for _, w := range weights {
		total += w
	}
	width := 170.0
	const lh = 5.2
	for ri, r := range rows {
		style := ""
		if ri == 0 {
			style = "B"
		}
		p.SetFont("Helvetica", style, 9.5)
		maxLines := 1
		cells := make([][]string, n)
		for c := 0; c < n; c++ {
			txt := ""
			if c < len(r) {
				txt = tr(clean(strings.ReplaceAll(r[c], "**", "")))
			}
			w := width*float64(weights[c])/float64(total) - 2
			var ls []string
			for _, b := range p.SplitText(txt, w) {
				ls = append(ls, string(b))
			}
			if len(ls) == 0 {
				ls = []string{""}
			}
			cells[c] = ls
			maxLines = max(maxLines, len(ls))
		}
		h := float64(maxLines)*lh + 2
		if p.GetY()+h > 277 {
			p.AddPage()
			p.SetFont("Helvetica", style, 9.5)
		}
		x, y := 20.0, p.GetY()
		for c := 0; c < n; c++ {
			w := width * float64(weights[c]) / float64(total)
			if ri == 0 {
				p.SetFillColor(225, 225, 225)
				p.Rect(x, y, w, h, "FD")
			} else {
				p.Rect(x, y, w, h, "D")
			}
			for li, s := range cells[c] {
				p.SetXY(x+1, y+1+float64(li)*lh)
				p.CellFormat(w-2, lh, s, "", 0, "L", false, 0, "")
			}
			x += w
		}
		p.SetXY(20, y+h)
	}
}

// Tree renders every .md file under root to a .pdf beside it. The PDF title
// is the file stem, which is also the document ID.
func Tree(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		src, err := os.ReadFile(path) //nolint:gosec // path comes from walking the corpus tree
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		out, err := PDF(strings.TrimSuffix(filepath.Base(path), ".md"), string(src))
		if err != nil {
			return err
		}
		if err := os.WriteFile(strings.TrimSuffix(path, ".md")+".pdf", out, 0o600); err != nil { //nolint:gosec // trusted local corpus tree
			return fmt.Errorf("write pdf for %s: %w", path, err)
		}
		return nil
	})
}
