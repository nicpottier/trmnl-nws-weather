package forecast

import (
	"math"
	"time"
)

// Wind is shown only when it's strong enough to matter: 6-hour bars hanging from the top of the
// chart, sized by the peak gust, and the day's peak gust beside its low and high. Below
// windyGustMph nothing is drawn, so calm days stay clean; a bar fills the wind band at
// fullGustMph. 40 mph is about where Stevens Pass starts holding its upper lifts.
const (
	windyGustMph = 25.0
	fullGustMph  = 60.0
	kmhPerMph    = 1.609
)

// fillWind sets the chart's wind bars and each day's peak gust (Day.Gust, display units, or 0
// when the day isn't windy). Blocks that have ended use the gusts measured at nearby stations.
func (c *Chart) fillWind(grid *GridData, days []Day, start, end, now time.Time, units string, seen *observed) {
	ivs := grid.Properties.WindGust.intervals()
	perDay := 24 / barHours
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
		if d := k / perDay; d < len(days) && mph >= windyGustMph {
			g := int(math.Round(mph))
			if units == "si" {
				g = int(math.Round(mph * kmhPerMph))
			}
			days[d].Gust = max(days[d].Gust, g)
		}
	}
}
