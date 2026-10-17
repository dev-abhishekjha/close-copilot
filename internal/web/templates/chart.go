package templates

import (
	"fmt"
	"hash/fnv"
	"math"
	"strconv"
	"unicode/utf8"
)

// Chart limits and geometry. Everything is drawn into a fixed viewBox and
// scaled by CSS, so the SVG carries attributes only (no style attribute).
const (
	chartMaxCategories = 24
	chartMaxSeries     = 4
	chartLabelRunes    = 14
	chartW             = 480.0
	chartH             = 240.0
	chartLeft          = 52.0
	chartRight         = 12.0
	chartTop           = 12.0
	chartBottom        = 44.0
	chartTicks         = 4
)

// Series is one named run of values, one per category.
type Series struct {
	Name   string
	Values []float64
}

// ChartProps describes BarChart and LineChart. Unit is a short suffix for
// the data table header (for example "tokens" or "USD").
type ChartProps struct {
	Title      string
	Desc       string
	Unit       string
	Series     []Series
	Categories []string
}

type chartRect struct{ X, Y, W, H, Class, Tip string }
type chartLine struct{ Points, Class string }
type chartDot struct{ CX, CY, Class, Tip string }
type chartTick struct{ Y, X1, X2, LX, LY, Label string }
type chartText struct{ X, Y, Text string }
type chartKey struct{ Class, Name string }
type chartTableRow struct {
	Label string
	Cells []string
}

// chartLayout is everything the template draws.
type chartLayout struct {
	Title, Desc, TitleID, DescID string
	ViewBox                      string
	Empty                        bool
	Ticks                        []chartTick
	Rects                        []chartRect
	Lines                        []chartLine
	Dots                         []chartDot
	XLabels                      []chartText
	Legend                       []chartKey
	Head                         []string
	Rows                         []chartTableRow
	Axis                         string // x-axis baseline y
	AxisX1, AxisX2               string
}

// cleanValue maps NaN and Inf to 0.
func cleanValue(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// round1 rounds to one decimal.
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// fmtGeom formats a coordinate with at most one decimal.
func fmtGeom(v float64) string { return strconv.FormatFloat(round1(v), 'f', -1, 64) }

// fmtNum formats a data value with up to six decimals and no trailing zeros.
func fmtNum(v float64) string {
	return strconv.FormatFloat(math.Round(cleanValue(v)*1e6)/1e6, 'f', -1, 64)
}

// niceMax returns the smallest 1, 2 or 5 times a power of ten that is at
// least v; 1 for zero or negative input.
func niceMax(v float64) float64 {
	v = cleanValue(v)
	if v <= 0 {
		return 1
	}
	exp := math.Pow(10, math.Floor(math.Log10(v)))
	f := v / exp
	for _, step := range []float64{1, 2, 5, 10} {
		if f <= step+1e-9 {
			return step * exp
		}
	}
	return 10 * exp
}

// cutLabel cuts s to chartLabelRunes runes, adding an ellipsis when it cut.
func cutLabel(s string) string {
	if utf8.RuneCountInString(s) <= chartLabelRunes {
		return s
	}
	return string([]rune(s)[:chartLabelRunes]) + "…"
}

func chartID(p ChartProps) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(p.Title + "\x00" + p.Desc))
	return fmt.Sprintf("chart-%08x", h.Sum32())
}

func seriesClass(kind string, i int) string {
	return "chart-" + kind + " chart-s" + strconv.Itoa(i)
}

// layoutChart computes the geometry for p; line selects polylines over bars.
func layoutChart(p ChartProps, line bool) chartLayout {
	id := chartID(p)
	l := chartLayout{
		Title: p.Title, Desc: p.Desc, TitleID: id + "-t", DescID: id + "-d",
		ViewBox: fmt.Sprintf("0 0 %s %s", fmtGeom(chartW), fmtGeom(chartH)),
	}
	cats := p.Categories
	if len(cats) > chartMaxCategories {
		cats = cats[:chartMaxCategories]
	}
	series := p.Series
	if len(series) > chartMaxSeries {
		series = series[:chartMaxSeries]
	}
	n := len(cats)
	// Clean copies, one value per category.
	vals := make([][]float64, len(series))
	maxV := 0.0
	for si, s := range series {
		vals[si] = make([]float64, n)
		for i := range n {
			if i < len(s.Values) {
				vals[si][i] = cleanValue(s.Values[i])
			}
			maxV = math.Max(maxV, vals[si][i])
		}
	}
	if n == 0 || len(series) == 0 || maxV <= 0 {
		l.Empty = true
		return l
	}
	top := niceMax(maxV)
	pw, ph := chartW-chartLeft-chartRight, chartH-chartTop-chartBottom
	base := chartTop + ph
	l.Axis, l.AxisX1, l.AxisX2 = fmtGeom(base), fmtGeom(chartLeft), fmtGeom(chartLeft+pw)
	for i := 0; i <= chartTicks; i++ {
		tv := top * float64(i) / chartTicks
		y := base - ph*float64(i)/chartTicks
		l.Ticks = append(l.Ticks, chartTick{
			Y: fmtGeom(y), X1: fmtGeom(chartLeft), X2: fmtGeom(chartLeft + pw),
			LX: fmtGeom(chartLeft - 6), LY: fmtGeom(y + 4), Label: fmtNum(tv),
		})
	}
	gw := pw / float64(n)
	for i, c := range cats {
		l.XLabels = append(l.XLabels, chartText{X: fmtGeom(chartLeft + gw*(float64(i)+0.5)), Y: fmtGeom(base + 16), Text: cutLabel(c)})
	}
	l.Head = append(l.Head, "Category")
	for _, s := range series {
		h := s.Name
		if p.Unit != "" {
			h += " (" + p.Unit + ")"
		}
		l.Head = append(l.Head, h)
	}
	for i, c := range cats {
		row := chartTableRow{Label: c}
		for si := range series {
			row.Cells = append(row.Cells, fmtNum(vals[si][i]))
		}
		l.Rows = append(l.Rows, row)
	}
	for si, s := range series {
		if len(series) > 1 {
			l.Legend = append(l.Legend, chartKey{Class: "chart-key chart-k" + strconv.Itoa(si), Name: s.Name})
		}
		if line {
			pts := ""
			for i := range n {
				x := chartLeft + gw*(float64(i)+0.5)
				y := base - round1(vals[si][i]/top*ph)
				if i > 0 {
					pts += " "
				}
				pts += fmtGeom(x) + "," + fmtGeom(y)
				l.Dots = append(l.Dots, chartDot{CX: fmtGeom(x), CY: fmtGeom(y), Class: seriesClass("dot", si), Tip: cats[i] + " " + s.Name + ": " + fmtNum(vals[si][i])})
			}
			l.Lines = append(l.Lines, chartLine{Points: pts, Class: seriesClass("line", si)})
			continue
		}
		bw := gw * 0.8 / float64(len(series))
		for i := range n {
			h := round1(vals[si][i] / top * ph)
			l.Rects = append(l.Rects, chartRect{
				X: fmtGeom(chartLeft + gw*0.1 + bw*float64(si) + gw*float64(i)), Y: fmtGeom(base - h),
				W: fmtGeom(bw), H: fmtGeom(h), Class: seriesClass("bar", si),
				Tip: cats[i] + " " + s.Name + ": " + fmtNum(vals[si][i]),
			})
		}
	}
	return l
}
