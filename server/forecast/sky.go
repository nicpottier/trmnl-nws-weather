package forecast

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// skySmoothHours is the window (centered) each hour's cloud cover is averaged over, and
// skyStepHours the spacing of the points the shape's edge is drawn through: hourly cover jumps
// around too much to read.
const (
	skySmoothHours = 7
	skyStepHours   = 3
)

// fillSky draws the forecast's cloud cover in the chart's top band, as a shape hanging from the top
// as deep as the sky is covered: SkyPath is the filled shape, SkyEdge its lower edge. It starts at
// now: none of the stations report clouds, so the past has none.
func (c *Chart) fillSky(grid *GridData, start, end, now time.Time) {
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

	hours := []time.Time{}
	cover := map[time.Time]float64{}
	for t := start; t.Before(end); t = t.Add(time.Hour) {
		if v, ok := coverAt(t.Add(30 * time.Minute)); ok {
			hours = append(hours, t)
			cover[t] = v
		}
	}
	xs, ys := []float64{}, []float64{}
	first := 0 // the first hour from now on; earlier hours still count in the averages
	for first < len(hours) && hours[first].Add(time.Hour).Before(now) {
		first++
	}
	for i := first; i < len(hours); i += skyStepHours {
		sum, n := 0.0, 0
		for d := -skySmoothHours / 2; d <= skySmoothHours/2; d++ {
			if v, ok := cover[hours[i].Add(time.Duration(d)*time.Hour)]; ok {
				sum, n = sum+v, n+1
			}
		}
		x := xOf(hours[i].Add(30 * time.Minute))
		if i == first {
			x = math.Max(xOf(now), xOf(hours[i]))
		}
		xs, ys = append(xs, x), append(ys, sum/float64(n))
	}
	if len(xs) < 2 {
		return
	}
	c.SkyEdge = smoothPath(xs, ys)
	c.SkyPath = "M" + fmtX(xs[0]) + ",0L" + strings.TrimPrefix(c.SkyEdge, "M") + "L" + fmtX(xs[len(xs)-1]) + ",0Z"
}

func fmtX(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }
