package templates

import (
	"context"
	"math"
	"strings"
	"testing"
)

func bar(t *testing.T, p ChartProps) string {
	t.Helper()
	var b strings.Builder
	if err := BarChart(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func line(t *testing.T, p ChartProps) string {
	t.Helper()
	var b strings.Builder
	if err := LineChart(p).Render(context.Background(), &b); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestChartGeometry(t *testing.T) {
	for in, want := range map[float64]float64{0: 1, -3: 1, 0.3: 0.5, 7: 10, 130: 200, 4200: 5000, 1: 1, 2: 2, 5: 5, 10: 10, math.NaN(): 1, math.Inf(1): 1} {
		if got := niceMax(in); got != want {
			t.Errorf("niceMax(%v) = %v, want %v", in, got, want)
		}
	}
	if cleanValue(math.NaN()) != 0 || cleanValue(math.Inf(-1)) != 0 || cleanValue(2.5) != 2.5 {
		t.Error("cleanValue")
	}
	if round1(1.26) != 1.3 || round1(1.24) != 1.2 || fmtGeom(10) != "10" || fmtGeom(3.14159) != "3.1" {
		t.Error("one-decimal rounding")
	}
	if cutLabel(strings.Repeat("é", 14)) != strings.Repeat("é", 14) || cutLabel(strings.Repeat("é", 15)) != strings.Repeat("é", 14)+"…" {
		t.Error("label cut")
	}

	var cats []string
	var series []Series
	for i := range 30 {
		cats = append(cats, string(rune('a'+i%26)))
	}
	for i := range 6 {
		v := make([]float64, 30)
		v[0] = float64(i + 1)
		series = append(series, Series{Name: string(rune('P' + i)), Values: v})
	}
	l := layoutChart(ChartProps{Title: "t", Categories: cats, Series: series}, false)
	if len(l.XLabels) != 24 || len(l.Rects) != 24*4 || len(l.Legend) != 4 || len(l.Rows) != 24 || len(l.Head) != 5 {
		t.Errorf("limits: labels %d rects %d legend %d rows %d head %d", len(l.XLabels), len(l.Rects), len(l.Legend), len(l.Rows), len(l.Head))
	}
	lw := layoutChart(ChartProps{Title: "t", Categories: []string{"x"}, Series: []Series{{Name: "s", Values: []float64{math.NaN()}}}}, false)
	if !lw.Empty {
		t.Error("NaN-only data must be empty")
	}
	l = layoutChart(ChartProps{Title: "t", Categories: []string{"x", "y"}, Series: []Series{{Name: "s", Values: []float64{math.Inf(1), 100}}}}, false)
	if l.Empty || l.Rows[0].Cells[0] != "0" || l.Rows[1].Cells[0] != "100" {
		t.Errorf("Inf handling: %+v", l.Rows)
	}
	if l.Rects[0].H != "0" || l.Rects[1].H != "184" { // 100 of nice max 100 fills the 184-high plot
		t.Errorf("heights = %s, %s", l.Rects[0].H, l.Rects[1].H)
	}
}

func TestChartBars(t *testing.T) {
	out := bar(t, ChartProps{Title: "Tokens", Desc: "d", Unit: "tokens", Categories: []string{"explain", "verify"},
		Series: []Series{{Name: "In", Values: []float64{10, 20}}, {Name: "Out", Values: []float64{5, 0}}}})
	for _, want := range []string{`<figure class="chart">`, `role="img"`, `viewBox="0 0 480 240"`, "<title id=", "<desc id=", "chart-s0", "chart-s1",
		"<summary>Data</summary>", "<th scope=\"col\">In (tokens)</th>", "<td>20</td>", "chart-legend", `class="chart-grid"`} {
		if !strings.Contains(out, want) {
			t.Errorf("bar chart lacks %q", want)
		}
	}
	if n := strings.Count(out, "<rect"); n != 4 {
		t.Errorf("rects = %d, want 4", n)
	}
	if strings.Contains(out, "<polyline") {
		t.Error("bar chart drew a line")
	}
}

func TestChartLine(t *testing.T) {
	out := line(t, ChartProps{Title: "Cost", Categories: []string{"a", "b", "c"}, Series: []Series{{Name: "USD", Values: []float64{1, 2, 3}}}})
	if strings.Count(out, "<polyline") != 1 || strings.Count(out, "<circle") != 3 || strings.Contains(out, "<rect") || strings.Contains(out, "chart-legend") {
		t.Errorf("line chart shape wrong:\n%s", out)
	}
}

func TestChartEmpty(t *testing.T) {
	for name, p := range map[string]ChartProps{
		"zeros":     {Title: "T", Categories: []string{"a"}, Series: []Series{{Name: "s", Values: []float64{0}}}},
		"negative":  {Title: "T", Categories: []string{"a"}, Series: []Series{{Name: "s", Values: []float64{-4}}}},
		"nothing":   {Title: "T"},
		"no series": {Title: "T", Categories: []string{"a"}},
	} {
		out := bar(t, p)
		if !strings.Contains(out, "No usage recorded") || strings.Contains(out, "chart-svg") || strings.Contains(out, "NaN") {
			t.Errorf("%s: %s", name, out)
		}
	}
}

func TestChartNoInlineStyle(t *testing.T) {
	evil := "<img onerror=x>"
	p := ChartProps{Title: "T" + evil, Desc: evil, Categories: []string{evil, `"q"`}, Series: []Series{{Name: evil, Values: []float64{3, 4}}}}
	for name, out := range map[string]string{"bar": bar(t, p), "line": line(t, p)} {
		for _, bad := range []string{"style=", "<script", "javascript:", "<img", "onerror=x>"} {
			if strings.Contains(out, bad) {
				t.Errorf("%s chart contains %q", name, bad)
			}
		}
		if !strings.Contains(out, "&lt;img onerror=x&gt;") {
			t.Errorf("%s chart does not escape the category", name)
		}
	}
}
