package retrieval

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func words(n int, prefix string) string {
	w := make([]string, n)
	for i := range w {
		w[i] = prefix + string(rune('a'+i%26))
	}
	return strings.Join(w, " ")
}

func TestSectionString(t *testing.T) {
	tests := []struct{ in, want string }{
		{"4. Prepaid expenses", "§4 Prepaid expenses"},
		{"4.2 Spread annual costs monthly", "§4.2 Spread annual costs monthly"},
		{"Calendar", "Calendar"},
		{"2026 plan", "§2026 plan"},
	}
	for _, tt := range tests {
		if got := sectionString(tt.in); got != tt.want {
			t.Errorf("sectionString(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestChunkMarkdownHeadings(t *testing.T) {
	md := "# My Title\n\nintro text\n\n## 4. Prepaid expenses\n\n### 4.2 Spread annual costs monthly\n\nbody two\n\n## Empty\n\n## Calendar\n\nbody cal\n"
	title, got := ChunkMarkdown(md)
	if title != "My Title" {
		t.Errorf("title = %q", title)
	}
	want := []Chunk{
		{Section: "", Content: "intro text"},
		{Section: "§4.2 Spread annual costs monthly", Content: "body two"},
		{Section: "Calendar", Content: "body cal"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("chunks = %#v, want %#v", got, want)
	}
}

func TestChunkMarkdownTitleRules(t *testing.T) {
	tests := []struct{ name, md, title string }{
		{"level two first", "## POL-001 Manual\n\n## 1. Scope\n\nx", "POL-001 Manual"},
		{"numbered first is a section", "## 1. Scope\n\nx", ""},
		{"text first", "hello\n\n## Calendar\n\nx", ""},
	}
	for _, tt := range tests {
		if title, _ := ChunkMarkdown(tt.md); title != tt.title {
			t.Errorf("%s: title = %q, want %q", tt.name, title, tt.title)
		}
	}
}

func TestChunkMarkdownLongSection(t *testing.T) {
	md := "# T\n\n## 1. Long\n\n" + words(900, "w") + "\n"
	_, got := ChunkMarkdown(md)
	if len(got) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(got))
	}
	for i, c := range got {
		if n := wordCount(c.Content); n > chunkWords+chunkOverlap {
			t.Errorf("chunk %d has %d words", i, n)
		}
		if c.Section != "§1 Long" {
			t.Errorf("chunk %d section %q", i, c.Section)
		}
	}
	a, b := strings.Fields(got[0].Content), strings.Fields(got[1].Content)
	if !reflect.DeepEqual(a[len(a)-chunkOverlap:], b[:chunkOverlap]) {
		t.Error("no overlap between chunks 0 and 1")
	}
}

func TestChunkMarkdownLongSectionParagraphs(t *testing.T) {
	md := "## T\n\n## 1. S\n\n" + words(200, "a") + "\n\n" + words(200, "b") + "\n\n" + words(200, "c") + "\n"
	_, got := ChunkMarkdown(md)
	if len(got) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(got))
	}
	for i, c := range got {
		if n := wordCount(c.Content); n > chunkWords+chunkOverlap {
			t.Errorf("chunk %d has %d words", i, n)
		}
	}
	if !strings.HasPrefix(got[1].Content, strings.Join(strings.Fields(got[0].Content)[150:], " ")) {
		t.Error("second chunk does not start with the tail of the first")
	}
}

func TestChunkMarkdownTableStaysWhole(t *testing.T) {
	var rows []string
	rows = append(rows, "| a | b |", "|---|---|")
	for i := 0; i < 200; i++ {
		rows = append(rows, "| x"+string(rune('a'+i%26))+" | y |")
	}
	table := strings.Join(rows, "\n")
	md := "## T\n\n## 1. S\n\n" + words(300, "p") + "\n\n" + table + "\n\n" + words(100, "q") + "\n"
	_, got := ChunkMarkdown(md)
	found := false
	for _, c := range got {
		if strings.Contains(c.Content, "| a | b |") {
			found = true
			if !strings.Contains(c.Content, rows[len(rows)-1]) {
				t.Error("table was split")
			}
			if strings.Contains(c.Content, "pa") {
				t.Error("oversized table should be its own chunk")
			}
		}
	}
	if !found {
		t.Fatal("table missing")
	}
}

func TestChunkMarkdownCodeFence(t *testing.T) {
	md := "## T\n\n## 1. S\n\nbefore\n\n```\n# not a heading\n```\n\nafter\n"
	_, got := ChunkMarkdown(md)
	if len(got) != 1 || !strings.Contains(got[0].Content, "# not a heading") {
		t.Fatalf("got %#v", got)
	}
}

func TestChunkMarkdownDeterministic(t *testing.T) {
	md := "## T\n\n## 1. S\n\n" + words(800, "z")
	_, a := ChunkMarkdown(md)
	_, b := ChunkMarkdown(md)
	if !reflect.DeepEqual(a, b) {
		t.Error("not deterministic")
	}
}

func TestChunkMarkdownDoclingSample(t *testing.T) {
	raw, err := os.ReadFile("testdata/docling_policy_sample.md")
	if err != nil {
		t.Fatal(err)
	}
	title, got := ChunkMarkdown(string(raw))
	if title != "POL-001 Accounting Policy Manual" {
		t.Errorf("title = %q", title)
	}
	sections := map[string]string{}
	for _, c := range got {
		sections[c.Section] += c.Content
	}
	for _, s := range []string{"", "§2.1 Working days", "§4.1 What is a prepaid expense", "§4.2 Spread annual costs monthly"} {
		if _, ok := sections[s]; !ok {
			t.Errorf("missing section %q in %v", s, sections)
		}
	}
	if _, ok := sections["§4 Prepaid expenses"]; ok {
		t.Error("empty section produced a chunk")
	}
	if !strings.Contains(sections["§2.1 Working days"], "|---------------|") {
		t.Error("table lost")
	}
}
