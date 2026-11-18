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
	fogs := grid.Properties.Weather.fog()
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
		for i := first; i < len(hours); i += skyStepHours {
			x := xOf(hours[i].Add(30 * time.Minute))
			if i == first {
				x = math.Max(xOf(now), xOf(hours[i]))
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

	// Where each 6-hour block's cloud ends, so rain and snow can fall from it: the cloud's depth at
	// the block's middle (the band is the plot's top quarter). Blocks already past have no clouds.
	c.BarTops = make([]float64, len(c.Bars))
	for k := range c.BarTops {
		mid := start.Add(time.Duration(k*barHours)*time.Hour + barHours*time.Hour/2).Truncate(time.Hour)
		if mid.Before(now) {
			continue
		}
		if _, ok := sky[mid]; ok {
			c.BarTops[k] = math.Round(cloudDepth(smoothed(sky, mid))*0.25*10) / 10
		}
	}
	c.FogPath, c.FogEdge = shape(fog)
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
