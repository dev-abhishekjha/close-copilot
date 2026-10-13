package retrieval

import (
	"regexp"
	"strings"
)

const (
	// chunkWords is the target size of a chunk in words.
	chunkWords = 350
	// chunkOverlap is how many words a split section repeats at the seam.
	chunkOverlap = 50
)

// Chunk is one retrievable passage of a document.
type Chunk struct {
	// Section is the citation path such as "§4.2 Spread annual costs monthly";
	// empty for text before the first heading.
	Section string
	// Content is the chunk body only, without the title or the section.
	Content string
}

var (
	headingRe  = regexp.MustCompile(`^\s{0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	numberedRe = regexp.MustCompile(`^(\d+(?:\.\d+)*)\.?\s+(\S.*)$`)
	fenceRe    = regexp.MustCompile("^\\s{0,3}(```|~~~)")
)

// sectionString turns a heading into a citation string: a numbered heading
// becomes "§<number> <text>" and any other heading is kept as is.
func sectionString(heading string) string {
	if m := numberedRe.FindStringSubmatch(heading); m != nil {
		return "§" + m[1] + " " + m[2]
	}
	return heading
}

type block struct {
	text  string
	table bool
}

type section struct {
	name   string
	blocks []block
}

// ChunkMarkdown splits Markdown on its headings. The title is the first
// heading when it is the first non-empty line and is not numbered (docling
// does not always keep the heading level), else "". Sections of at most about
// 350 words are one chunk; longer ones are split with about 50 words of
// overlap. A table is never split. It is pure and deterministic.
func ChunkMarkdown(md string) (title string, chunks []Chunk) {
	lines := strings.Split(strings.ReplaceAll(md, "\r\n", "\n"), "\n")
	var sections []section
	cur := section{}
	var para []string
	var table []string
	inFence := false
	seenContent := false

	flushPara := func() {
		if len(para) > 0 {
			cur.blocks = append(cur.blocks, block{text: strings.Join(para, "\n")})
			para = nil
		}
	}
	flushTable := func() {
		if len(table) > 0 {
			cur.blocks = append(cur.blocks, block{text: strings.Join(table, "\n"), table: true})
			table = nil
		}
	}

	for _, line := range lines {
		if fenceRe.MatchString(line) {
			inFence = !inFence
			flushTable()
			para = append(para, line)
			seenContent = true
			continue
		}
		if inFence {
			para = append(para, line)
			continue
		}
		if m := headingRe.FindStringSubmatch(line); m != nil {
			flushPara()
			flushTable()
			text := strings.TrimSpace(m[2])
			if !seenContent && title == "" && numberedRe.FindString(text) == "" {
				seenContent = true
				title = text
				continue
			}
			seenContent = true
			sections = append(sections, cur)
			cur = section{name: sectionString(text)}
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			flushPara()
			flushTable()
		case strings.HasPrefix(trimmed, "|"):
			flushPara()
			table = append(table, trimmed)
			seenContent = true
		default:
			flushTable()
			para = append(para, line)
			seenContent = true
		}
	}
	flushPara()
	flushTable()
	sections = append(sections, cur)

	for _, s := range sections {
		for _, c := range packSection(s.blocks) {
			chunks = append(chunks, Chunk{Section: s.name, Content: c})
		}
	}
	return title, chunks
}

func wordCount(s string) int { return len(strings.Fields(s)) }

// packSection packs the blocks of one section into chunk bodies.
func packSection(blocks []block) []string {
	total := 0
	for _, b := range blocks {
		total += wordCount(b.text)
	}
	if total == 0 {
		return nil
	}
	if total <= chunkWords {
		parts := make([]string, len(blocks))
		for i, b := range blocks {
			parts[i] = strings.TrimSpace(b.text)
		}
		return []string{strings.Join(parts, "\n\n")}
	}

	var out []string
	var cur []string
	curWords := 0
	curNew := false // cur holds more than the overlap seed
	lastText := ""  // last emitted chunk when it ended in text, for overlap

	flush := func() {
		if len(cur) > 0 && curNew {
			text := strings.Join(cur, "\n\n")
			out = append(out, text)
			lastText = text
		}
		cur, curWords, curNew = nil, 0, false
	}

	for _, b := range blocks {
		text := strings.TrimSpace(b.text)
		w := wordCount(text)
		if w == 0 {
			continue
		}
		switch {
		case b.table:
			if curWords+w > chunkWords {
				flush()
				out = append(out, text)
				lastText = ""
				continue
			}
		case w > chunkWords:
			flush()
			words := strings.Fields(text)
			for start := 0; start < len(words); {
				end := min(start+chunkWords, len(words))
				out = append(out, strings.Join(words[start:end], " "))
				if end == len(words) {
					break
				}
				start = end - chunkOverlap
			}
			lastText = out[len(out)-1]
			continue
		case curWords+w > chunkWords:
			flush()
			if lastText != "" {
				ws := strings.Fields(lastText)
				seed := strings.Join(ws[max(0, len(ws)-chunkOverlap):], " ")
				cur = []string{seed}
				curWords = wordCount(seed)
			}
		}
		cur = append(cur, text)
		curWords += w
		curNew = true
	}
	flush()
	return out
}
