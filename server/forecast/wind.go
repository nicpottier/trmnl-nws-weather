package forecast

import (
	"fmt"
	"math"
	"time"
)

// Wind is shown only when it's strong enough to matter: 6-hour bars hanging from the top of the
// chart, sized by the peak gust, and the day's peak gust beside its low and high. Below
// windyGustMph nothing is drawn, so calm days stay clean; a bar fills the wind band at
// fullGustMph. 40 mph is about where Stevens Pass starts holding its upper lifts.
const (
	windyGustMph     = 25.0
	fullGustMph      = 60.0
	veryWindyGustMph = 40.0
	kmhPerMph        = 1.609
)

// fillWind sets the chart's wind bars and each day's peak gust (Day.Gust, display units, or 0
// when the day isn't windy). Blocks that have ended use the gusts measured at nearby stations.
func (c *Chart) fillWind(grid *GridData, days []Day, start, end, now time.Time, units string, seen *observed) {
	ivs := grid.Properties.WindGust.intervals()
	perDay := 24 / barHours
	gusts := []float64{}
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
		h := 0.0
		if mph >= windyGustMph {
			h = math.Min(100, math.Round(1000*mph/fullGustMph)/10)
		}
		c.Wind = append(c.Wind, h)
		gusts = append(gusts, mph)
		level := 0
		switch {
		case mph >= veryWindyGustMph:
			level = 2
		case mph >= windyGustMph:
			level = 1
		}
		c.WindLevel = append(c.WindLevel, level)
		if d := k / perDay; d < len(days) && mph >= windyGustMph {
			g := int(math.Round(mph))
			if units == "si" {
				g = int(math.Round(mph * kmhPerMph))
			}
			days[d].Gust = max(days[d].Gust, g)
		}
	}

	c.WindArea = hangingPath(c.Wind)
	n := float64(len(c.Wind))
	for _, r := range runs(len(c.Wind), func(k int) bool { return c.Wind[k] > 0 }) {
		x := func(k float64) float64 { return math.Round(k/n*10000) / 10 }
		c.WindLine += fmt.Sprintf("M%g,%g", x(float64(r[0])), c.Wind[r[0]])
		for k := r[0]; k <= r[1]; k++ {
			c.WindLine += fmt.Sprintf("L%g,%g", x(float64(k)+0.5), c.Wind[k])
		}
		c.WindLine += fmt.Sprintf("L%g,%g", x(float64(r[1]+1)), c.Wind[r[1]])
	}

	// One label per windy spell: its peak gust, centered under it.
	c.Gusts = []Storm{}
	for _, r := range runs(len(gusts), func(k int) bool { return gusts[k] >= windyGustMph }) {
		peak := 0.0
		for _, g := range gusts[r[0] : r[1]+1] {
			peak = math.Max(peak, g)
		}
		label := fmt.Sprintf("%.0f mph", peak)
		if units == "si" {
			label = fmt.Sprintf("%.0f km/h", peak*kmhPerMph)
		}
		c.Gusts = append(c.Gusts, Storm{X: runCenter(r, len(gusts)), Labels: []string{label}})
	}
}
