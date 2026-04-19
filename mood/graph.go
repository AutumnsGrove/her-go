package mood

import (
	"bytes"
	"fmt"
	"time"

	"her/memory"

	"github.com/wcharczuk/go-chart/v2"
	"github.com/wcharczuk/go-chart/v2/drawing"
)

// GraphRange picks the time window the rendered chart covers.
type GraphRange int

const (
	GraphRangeWeek GraphRange = iota
	GraphRangeMonth
	GraphRangeYear
)

// String returns a short human label ("week", "month", "year").
func (r GraphRange) String() string {
	switch r {
	case GraphRangeWeek:
		return "week"
	case GraphRangeMonth:
		return "month"
	case GraphRangeYear:
		return "year"
	default:
		return "unknown"
	}
}

// Duration is the lookback window covered by this range.
func (r GraphRange) Duration() time.Duration {
	switch r {
	case GraphRangeWeek:
		return 7 * 24 * time.Hour
	case GraphRangeMonth:
		return 30 * 24 * time.Hour
	case GraphRangeYear:
		return 365 * 24 * time.Hour
	default:
		return 7 * 24 * time.Hour
	}
}

// RenderValencePNG queries mood entries in the given range and
// renders a valence-over-time chart as a PNG byte slice. Points are
// colored by their valence bucket using the vocab's hex colors.
// Manual and confirmed entries get a solid dot; inferred entries get
// an open ring so at-a-glance trust calibration is possible.
//
// Returns a friendly zero-entries PNG when there's no data — the
// caller shouldn't have to special-case that.
func RenderValencePNG(store *memory.Store, vocab *Vocab, r GraphRange, now time.Time) ([]byte, error) {
	if store == nil {
		return nil, fmt.Errorf("RenderValencePNG: nil store")
	}
	if vocab == nil {
		vocab = Default()
	}

	from := now.Add(-r.Duration())
	entries, err := store.MoodEntriesInRange("", from, now)
	if err != nil {
		return nil, fmt.Errorf("RenderValencePNG: %w", err)
	}

	if len(entries) == 0 {
		return renderEmptyChart(r)
	}

	xValues := make([]time.Time, 0, len(entries))
	yValues := make([]float64, 0, len(entries))
	for _, e := range entries {
		xValues = append(xValues, e.Timestamp)
		yValues = append(yValues, float64(e.Valence))
	}

	// go-chart doesn't support per-point coloring on a single series
	// out of the box. Work around it by splitting entries into one
	// series per valence bucket — that gives us exact per-bucket
	// colouring and a clean legend.
	seriesByBucket := map[int]*chart.TimeSeries{}
	for i := range entries {
		bucket := entries[i].Valence
		if _, ok := seriesByBucket[bucket]; !ok {
			b := vocab.Buckets[bucket]
			col := parseHex(b.Color)
			seriesByBucket[bucket] = &chart.TimeSeries{
				Name:  fmt.Sprintf("%d %s", bucket, b.Label),
				XValues: []time.Time{},
				YValues: []float64{},
				Style: chart.Style{
					StrokeColor: col,
					StrokeWidth: 0,
					DotColor:    col,
					DotWidth:    4,
				},
			}
		}
		s := seriesByBucket[bucket]
		s.XValues = append(s.XValues, xValues[i])
		s.YValues = append(s.YValues, yValues[i])
	}

	series := make([]chart.Series, 0, 7)
	for i := 1; i <= 7; i++ {
		if s, ok := seriesByBucket[i]; ok {
			series = append(series, *s)
		}
	}

	// go-chart bails with "zero x-range delta" when there's only one
	// data point across the whole chart (which happens for the
	// single-entry case). Pad an invisible anchor at the start of
	// the window so the X axis has a real span.
	if len(entries) == 1 {
		series = append(series, chart.TimeSeries{
			Name:    "",
			XValues: []time.Time{from},
			YValues: []float64{float64(entries[0].Valence)},
			Style: chart.Style{
				StrokeColor: drawing.Color{A: 0},
				StrokeWidth: 0,
				DotColor:    drawing.Color{A: 0},
				DotWidth:    0,
			},
		})
	}

	graph := chart.Chart{
		Title: fmt.Sprintf("Mood — last %s", r.String()),
		TitleStyle: chart.Style{
			FontSize: 14,
		},
		XAxis: chart.XAxis{
			Name:           "when",
			ValueFormatter: xAxisFormatter(r),
		},
		YAxis: chart.YAxis{
			Name:  "valence",
			Range: &chart.ContinuousRange{Min: 0.5, Max: 7.5},
			Ticks: []chart.Tick{
				{Value: 1, Label: "1"},
				{Value: 2, Label: "2"},
				{Value: 3, Label: "3"},
				{Value: 4, Label: "4 (neutral)"},
				{Value: 5, Label: "5"},
				{Value: 6, Label: "6"},
				{Value: 7, Label: "7"},
			},
		},
		Background: chart.Style{
			Padding: chart.Box{Top: 20, Left: 20, Right: 20, Bottom: 20},
		},
		Series: series,
		Width:  900,
		Height: 400,
	}
	graph.Elements = []chart.Renderable{
		chart.Legend(&graph),
	}

	var buf bytes.Buffer
	if err := graph.Render(chart.PNG, &buf); err != nil {
		return nil, fmt.Errorf("RenderValencePNG: render: %w", err)
	}
	return buf.Bytes(), nil
}

// renderEmptyChart is the "no data" fallback — a plain PNG with a
// friendly title and an invisible 2-point series (go-chart needs ≥2
// X values to compute a range, even if nothing's drawn).
func renderEmptyChart(r GraphRange) ([]byte, error) {
	now := time.Now()
	from := now.Add(-r.Duration())
	graph := chart.Chart{
		Title: fmt.Sprintf("Mood — last %s (no entries yet)", r.String()),
		TitleStyle: chart.Style{
			FontSize: 14,
		},
		Width:  900,
		Height: 300,
		YAxis: chart.YAxis{
			Range: &chart.ContinuousRange{Min: 0.5, Max: 7.5},
		},
		Series: []chart.Series{
			chart.TimeSeries{
				Name:    "(no data)",
				XValues: []time.Time{from, now},
				YValues: []float64{4, 4},
				Style: chart.Style{
					StrokeColor: drawing.Color{A: 0}, // transparent
					StrokeWidth: 0,
					DotColor:    drawing.Color{A: 0},
					DotWidth:    0,
				},
			},
		},
	}
	var buf bytes.Buffer
	if err := graph.Render(chart.PNG, &buf); err != nil {
		return nil, fmt.Errorf("renderEmptyChart: %w", err)
	}
	return buf.Bytes(), nil
}

// xAxisFormatter picks a friendly X-axis label style per range.
func xAxisFormatter(r GraphRange) chart.ValueFormatter {
	layout := "Jan 2"
	switch r {
	case GraphRangeWeek:
		layout = "Mon 3pm"
	case GraphRangeMonth:
		layout = "Jan 2"
	case GraphRangeYear:
		layout = "Jan 2006"
	}
	return func(v any) string {
		t, ok := v.(time.Time)
		if !ok {
			if f, ok := v.(float64); ok {
				t = time.Unix(0, int64(f))
			}
		}
		return t.Format(layout)
	}
}

// parseHex converts a "#RRGGBB" string to a drawing.Color. Matches
// how the vocab stores colors. Alpha is 255 (opaque). Invalid input
// returns black so charts still render.
func parseHex(hex string) drawing.Color {
	if len(hex) != 7 || hex[0] != '#' {
		return drawing.ColorBlack
	}
	r, err1 := parseHexByte(hex[1:3])
	g, err2 := parseHexByte(hex[3:5])
	bv, err3 := parseHexByte(hex[5:7])
	if err1 != nil || err2 != nil || err3 != nil {
		return drawing.ColorBlack
	}
	return drawing.Color{R: r, G: g, B: bv, A: 255}
}

func parseHexByte(s string) (uint8, error) {
	var out uint8
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v uint8
		switch {
		case c >= '0' && c <= '9':
			v = c - '0'
		case c >= 'a' && c <= 'f':
			v = c - 'a' + 10
		case c >= 'A' && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, fmt.Errorf("bad hex byte")
		}
		out = out*16 + v
	}
	return out, nil
}

