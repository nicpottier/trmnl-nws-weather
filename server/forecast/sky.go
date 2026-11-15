package forecast

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// fillSky draws the forecast's cloud cover in the chart's top band: as a shape hanging from the top,
// as deep as the sky is covered (SkyPath), and as one level per 6-hour block (Sky).
func (c *Chart) fillSky(grid *GridData, start, end time.Time) {
	ivs := grid.Properties.SkyCover.intervals()
	coverAt := func(t time.Time) (float64, bool) {
		for _, iv := range ivs {
			if !t.Before(iv.start) && t.Before(iv.end) {
				return iv.value, true
			}
		}
		return 0, false
	}
	span := float64(end.Sub(start))
	xOf := func(t time.Time) float64 { return float64(t.Sub(start)) / span * 1000 }

	// The shape: one point per hour at the hour's middle, over the hours the forecast covers.
	xs, ys := []float64{}, []float64{}
	for t := start; t.Before(end); t = t.Add(time.Hour) {
		if v, ok := coverAt(t.Add(30 * time.Minute)); ok {
			xs, ys = append(xs, xOf(t.Add(30*time.Minute))), append(ys, v)
		}
	}
	if len(xs) > 1 {
		line := smoothPath(xs, ys)
		c.SkyPath = "M" + fmtX(xs[0]) + ",0L" + strings.TrimPrefix(line, "M") + "L" + fmtX(xs[len(xs)-1]) + ",0Z"
	}

	for t := start; t.Before(end); t = t.Add(barHours * time.Hour) {
		sum, n := 0.0, 0
		for h := t; h.Before(t.Add(barHours * time.Hour)); h = h.Add(time.Hour) {
			if v, ok := coverAt(h.Add(30 * time.Minute)); ok {
				sum, n = sum+v, n+1
			}
		}
		level := -1
		if n > 0 {
			switch avg := sum / float64(n); {
			case avg < 30:
				level = 0
			case avg < 70:
				level = 1
			default:
				level = 2
			}
		}
		c.Sky = append(c.Sky, level)
	}
}

func fmtX(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }
