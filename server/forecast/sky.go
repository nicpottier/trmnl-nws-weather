package forecast

import (
	"fmt"
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
// as deep as the sky is covered beyond clear (see cloudDepth): CloudPath and CloudEdge for the sky
// cover, FogPath and FogEdge for forecast fog, which sky cover leaves out. The sun (skyHours) goes by
// the two together. It starts at
// now: none of the stations report clouds, so the past has none.
func (c *Chart) fillSky(grid *GridData, start, end, now time.Time) {
	ivs := grid.Properties.SkyCover.intervals()
	skyAt := func(t time.Time) (float64, bool) {
		for _, iv := range ivs {
			if !t.Before(iv.start) && t.Before(iv.end) {
				return iv.value, true
			}
		}
		return 0, false
	}
	fogs := append(grid.Properties.Weather.fog(), grid.pastFog...)
	span := float64(end.Sub(start))
	xOf := func(t time.Time) float64 { return float64(t.Sub(start)) / span * 1000 }

	// Per hour: the sky cover, the fog (as cover), and the two together. Sky cover leaves out fog,
	// so a foggy morning would otherwise look clear.
	hours := []time.Time{}
	sky, fog, both := map[time.Time]float64{}, map[time.Time]float64{}, map[time.Time]float64{}
	for t := start; t.Before(end); t = t.Add(time.Hour) {
		mid := t.Add(30 * time.Minute)
		v, ok := skyAt(mid)
		if !ok {
			continue
		}
		hours = append(hours, t)
		f := fogs.at(mid)
		sky[t], fog[t], both[t] = v, f, math.Max(v, f)
	}
	first := 0 // the first hour from now on; earlier hours still count in the averages
	for first < len(hours) && hours[first].Add(time.Hour).Before(now) {
		first++
	}
	smoothed := func(cover map[time.Time]float64, h time.Time) float64 {
		sum, n := 0.0, 0
		for d := -skySmoothHours / 2; d <= skySmoothHours/2; d++ {
			if v, ok := cover[h.Add(time.Duration(d)*time.Hour)]; ok {
				sum, n = sum+v, n+1
			}
		}
		return sum / float64(n)
	}
	c.skyHours = nil
	for _, h := range hours[first:] {
		c.skyHours = append(c.skyHours, skyHour{h, smoothed(both, h)})
	}
	shape := func(cover map[time.Time]float64) (path, edge string) {
		xs, ys := []float64{}, []float64{}
		// Past hours too: they come from the forecast saved as each hour arrived (skyhistory.go).
		for i := 0; i < len(hours); i += skyStepHours {
			x := xOf(hours[i].Add(30 * time.Minute))
			if i == 0 {
				x = xOf(hours[i])
			}
			xs, ys = append(xs, x), append(ys, cloudDepth(smoothed(cover, hours[i])))
		}
		// One shape per covered stretch, each easing in from and out to the clear points beside it, so
		// clear sky is left empty (no edge drawn along the top).
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
				e := smoothPath(xs[from:to+1], ys[from:to+1])
				edge += e
				path += "M" + fmtX(xs[from]) + ",0L" + strings.TrimPrefix(e, "M") + "L" + fmtX(xs[to]) + ",0Z"
			}
			i = to
		}
		return path, edge
	}
	c.CloudPath, c.CloudEdge = shape(sky)
	c.FogPath, c.FogEdge = shape(fog)

	c.fillFalling(start, end, now, func(h time.Time) float64 {
		if _, ok := sky[h]; !ok { // no clouds known (before the saved history): fall from the top
			return 0
		}
		return cloudDepth(smoothed(sky, h)) * skyBandPct / 100
	})
}

// WeatherSeries is the grid's "weather" layer: per interval, the expected conditions (fog, rain,
// snow, ...) and how widespread they are.
type WeatherSeries struct {
	Values []struct {
		ValidTime string `json:"validTime"`
		Value     []struct {
			Coverage string `json:"coverage"`
			Weather  string `json:"weather"`
		} `json:"value"`
	} `json:"values"`
}

// fogCover maps the forecast's fog coverage to an equivalent cloud cover (%).
var fogCover = map[string]float64{
	"patchy": 50, "areas": 75, "widespread": 100, "definite": 100,
	"slight_chance": 30, "chance": 50, "likely": 75,
}

type fogIntervals []interval

// fog returns the intervals with fog (or freezing fog), valued by fogCover.
func (w WeatherSeries) fog() fogIntervals {
	out := fogIntervals{}
	for _, v := range w.Values {
		start, end, err := parseInterval(v.ValidTime)
		if err != nil {
			continue
		}
		cover := 0.0
		for _, c := range v.Value {
			if c.Weather == "fog" || c.Weather == "freezing_fog" {
				f, ok := fogCover[c.Coverage]
				if !ok {
					f = 75
				}
				cover = math.Max(cover, f)
			}
		}
		if cover > 0 {
			out = append(out, interval{start, end, cover})
		}
	}
	return out
}

func (f fogIntervals) at(t time.Time) float64 {
	for _, iv := range f {
		if !t.Before(iv.start) && t.Before(iv.end) {
			return iv.value
		}
	}
	return 0
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

// skyBandPct is the top band's share of the plot height (the clouds, fog, sun and wind live there).
const skyBandPct = 25.0

// fallPct is how far rain and snow fall below the clouds, in % of the plot: the same for any amount,
// since the bars below give the amounts.
const fallPct = 9.0

// fillFalling draws rain and snow falling from the clouds: for each run of 6-hour blocks with the
// same kind of precipitation, the marks (see fallingMarks) inside an area whose top follows the
// clouds' lower edge (cloudBottom, in % of the plot, by hour) and that hangs fallPct below it,
// tapering where the precipitation starts and stops.
func (c *Chart) fillFalling(start, end, now time.Time, cloudBottom func(time.Time) float64) {
	c.RainPath, c.SnowPath = "", ""
	span := float64(end.Sub(start))
	xOf := func(t time.Time) float64 { return float64(t.Sub(start)) / span * 1000 }
	kind := func(k int) string {
		if k < 0 || k >= len(c.Bars) {
			return ""
		}
		return c.Bars[k].Kind
	}
	for k := 0; k < len(c.Bars); k++ {
		kd := kind(k)
		if kd == "" {
			continue
		}
		to := k
		for kind(to+1) == kd {
			to++
		}
		from, until := start.Add(time.Duration(k*barHours)*time.Hour), start.Add(time.Duration((to+1)*barHours)*time.Hour)
		taperIn, taperOut := kind(k-1) == "", kind(to+1) == ""
		xs, tops, bottoms := []float64{}, []float64{}, []float64{}
		for t := from; !t.After(until); t = t.Add(time.Hour) {
			h := t
			if !h.Before(until) {
				h = until.Add(-time.Hour)
			}
			top := cloudBottom(h)
			length := fallPct
			if in := t.Sub(from).Hours(); taperIn && in < 3 {
				length *= 0.25 + 0.75*in/3
			}
			if out := until.Sub(t).Hours(); taperOut && out < 3 {
				length *= 0.25 + 0.75*out/3
			}
			xs, tops, bottoms = append(xs, xOf(t)), append(tops, top), append(bottoms, top+length)
		}
		marks := fallingMarks(kd, xs, tops, bottoms)
		if kd == "snow" {
			c.SnowPath += marks
		} else {
			c.RainPath += marks
		}
		k = to
	}
}

// fallingMarks lays rain dashes or snow dots on a fixed grid, like a pattern fill (so they line
// up across the chart), and keeps the whole marks that start inside the area between tops and
// bottoms (by x, in plot units) instead of clipping them: none is cut in half at an edge. The result is SVG path data to
// stroke: a dash per raindrop, a zero-length segment per snowflake (a round dot by its round cap).
func fallingMarks(kind string, xs, tops, bottoms []float64) string {
	if len(xs) < 2 {
		return ""
	}
	at := func(vals []float64, x float64) float64 {
		for i := 1; i < len(xs); i++ {
			if x <= xs[i] {
				f := (x - xs[i-1]) / (xs[i] - xs[i-1])
				return vals[i-1] + f*(vals[i]-vals[i-1])
			}
		}
		return vals[len(vals)-1]
	}
	clear := func(x, y float64) bool { return x >= xs[0] && x <= xs[len(xs)-1] && y >= at(tops, x) }
	inside := func(x, y float64) bool { return clear(x, y) && y <= at(bottoms, x) }
	// One grid cell's marks, each from (x, y) to (x+dx, y+dy); pad keeps their round ends inside.
	type mark struct{ x, y, dx, dy float64 }
	cellW, cellH, pad, cell := 14.0, 9.0, 0.5, []mark{{9, 1, -2, 3.5}, {2, 5.5, -2, 3.5}}
	if kind == "snow" {
		cellW, cellH, pad, cell = 14.0, 8.0, 1.0, []mark{{4, 2, 0, 0}, {11, 6, 0, 0}}
	}
	top, bottom := tops[0], bottoms[0]
	for i := range xs {
		top, bottom = math.Min(top, tops[i]), math.Max(bottom, bottoms[i])
	}
	var b strings.Builder
	for cx := math.Floor(xs[0]/cellW) * cellW; cx <= xs[len(xs)-1]; cx += cellW {
		for cy := math.Floor(top/cellH) * cellH; cy <= bottom; cy += cellH {
			for _, m := range cell {
				x0, y0, x1, y1 := cx+m.x, cy+m.y, cx+m.x+m.dx, cy+m.y+m.dy
				// Whole marks only: clear of the clouds at the top, and starting inside the area (the
				// bottom edge isn't drawn, so a mark may finish a little past it).
				if !clear(x0, y0-pad) || !clear(x1, y1-pad) || !inside(x0, y0) {
					continue
				}
				if kind == "snow" {
					fmt.Fprintf(&b, "M%s %sh0", fmtX(x0), fmtX(y0))
				} else {
					fmt.Fprintf(&b, "M%s %sL%s %s", fmtX(x0), fmtX(y0), fmtX(x1), fmtX(y1))
				}
			}
		}
	}
	return b.String()
}
