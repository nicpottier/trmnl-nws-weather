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
// as deep as the sky is covered beyond clear (see cloudDepth): SkyPath is the filled shape, SkyEdge
// its lower edge. It starts at
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
		xs, ys = append(xs, x), append(ys, cloudDepth(sum/float64(n)))
	}

	// One shape per cloudy stretch, each easing in from and out to the clear points beside it, so
	// clear sky is left empty (no edge drawn along the top).
	c.SkyEdge, c.SkyPath = "", ""
	for i := 0; i < len(xs); i++ {
		if ys[i] == 0 {
			continue
		}
		from, to := max(i-1, 0), i
		for to+1 < len(xs) && ys[to+1] > 0 {
			to++
		}
		to = min(to+1, len(xs)-1)
		if to > from {
			edge := smoothPath(xs[from:to+1], ys[from:to+1])
			c.SkyEdge += edge
			c.SkyPath += "M" + fmtX(xs[from]) + ",0L" + strings.TrimPrefix(edge, "M") + "L" + fmtX(xs[to]) + ",0Z"
		}
		i = to
	}
}

// clearSkyCover is the cloud cover (%) below which the sky counts as clear and nothing is drawn.
const clearSkyCover = 25.0

// cloudDepth maps cloud cover to how far the shape hangs into the band (0-100): nothing when clear,
// then in proportion from clearSkyCover to overcast, so it grows from nothing rather than jumping.
func cloudDepth(cover float64) float64 {
	if cover < clearSkyCover {
		return 0
	}
	return math.Round((cover-clearSkyCover)/(100-clearSkyCover)*1000) / 10
}

func fmtX(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }
