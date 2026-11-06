package forecast

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Wind is shown only when it's strong enough to matter: a dotted curve hanging from the top of
// the chart for each windy spell, deeper for stronger gusts, and the spell's peak gust under the
// chart. Below windyGustMph nothing is drawn, so calm days stay clean; the curve reaches the
// bottom of the wind band at fullGustMph. 40 mph is about where Stevens Pass starts holding its
// upper lifts.
const (
	windyGustMph = 25.0
	fullGustMph  = 60.0
	kmhPerMph    = 1.609
)

// fillWind sets the chart's wind curve and labels and each day's peak gust (Day.Gust, display
// units, or 0 when the day isn't windy). Blocks that have ended use the gusts measured at nearby
// stations.
func (c *Chart) fillWind(grid *GridData, days []Day, start, end, now time.Time, units string, seen *observed) {
	ivs := grid.Properties.WindGust.intervals()
	perDay := 24 / barHours
	gusts := []float64{} // mph, per 6-hour block
	for k, t := 0, start; t.Before(end); k, t = k+1, t.Add(barHours*time.Hour) {
		next := t.Add(barHours * time.Hour)
		mph := 0.0
		for _, iv := range ivs { // km/h
			if iv.start.Before(next) && iv.end.After(t) {
				mph = math.Max(mph, iv.value/kmhPerMph)
			}
		}
		if seen != nil && len(seen.gusts) > 0 && !next.After(now) {
			mph = seen.gust(t, next)
		}
		gusts = append(gusts, mph)
		if d := k / perDay; d < len(days) && mph >= windyGustMph {
			g := int(math.Round(mph))
			if units == "si" {
				g = int(math.Round(mph * kmhPerMph))
			}
			days[d].Gust = max(days[d].Gust, g)
		}
	}

	// Each windy spell: a curve that eases in from zero a whole block before the spell's first
	// block (at the middle of the calm block before it), runs through each block's gust at the
	// block's middle, and eases back out to zero a block after its last; and one label with its
	// peak, centered under it. The ends clamp to the chart's edges.
	c.Gusts = []Storm{}
	n := float64(len(gusts))
	for _, r := range runs(len(gusts), func(k int) bool { return gusts[k] >= windyGustMph }) {
		xs, ys := []float64{math.Max(0, (float64(r[0])-0.5)/n*1000)}, []float64{0}
		peak := 0.0
		for k := r[0]; k <= r[1]; k++ {
			xs = append(xs, (float64(k)+0.5)/n*1000)
			ys = append(ys, math.Min(100, 100*gusts[k]/fullGustMph))
			peak = math.Max(peak, gusts[k])
		}
		xs, ys = append(xs, math.Min(1000, (float64(r[1])+1.5)/n*1000)), append(ys, 0)
		c.WindLine += smoothPath(xs, ys)

		label := fmt.Sprintf("%.0f mph", peak)
		if units == "si" {
			label = fmt.Sprintf("%.0f km/h", peak*kmhPerMph)
		}
		c.Gusts = append(c.Gusts, Storm{X: runCenter(r, len(gusts)), Labels: []string{label}})
	}
}

// smoothPath draws a monotone cubic through the points, as an SVG path starting with M. It passes
// through every point without overshooting, so it never goes above zero or past the strongest gust.
func smoothPath(xs, ys []float64) string {
	m := len(xs)
	d := make([]float64, m-1)
	for i := range d {
		d[i] = (ys[i+1] - ys[i]) / (xs[i+1] - xs[i])
	}
	t := make([]float64, m) // tangents (Fritsch-Carlson); flat at peaks and at the ends
	for i := 1; i < m-1; i++ {
		if d[i-1]*d[i] > 0 {
			t[i] = 3 * (xs[i+1] - xs[i-1]) / ((2*xs[i+1]-xs[i]-xs[i-1])/d[i-1] + (xs[i+1]-2*xs[i]+xs[i-1])/d[i])
		}
	}
	f := func(v float64) string { return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64) }
	var b strings.Builder
	b.WriteString("M" + f(xs[0]) + "," + f(ys[0]))
	for i := 0; i < m-1; i++ {
		h := xs[i+1] - xs[i]
		fmt.Fprintf(&b, "C%s,%s %s,%s %s,%s", f(xs[i]+h/3), f(ys[i]+t[i]*h/3),
			f(xs[i+1]-h/3), f(ys[i+1]-t[i+1]*h/3), f(xs[i+1]), f(ys[i+1]))
	}
	return b.String()
}
