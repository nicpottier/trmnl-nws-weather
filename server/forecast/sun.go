package forecast

import (
	"math"
	"sort"
	"time"
)

type skyHour struct {
	t     time.Time
	cover float64 // percent, smoothed
}

// A sun peeks down from the top of the chart over each day's longest clear, sunny stretch, when it
// lasts at least minSunnyHours: daylight hours with cloud cover under clearSkyCover.
const minSunnyHours = 3

// placeSuns sets Suns from the smoothed cloud cover and the sun's height at the location.
func (c *Chart) placeSuns(lat, lon float64, loc *time.Location) {
	c.Suns = []float64{}
	span := float64(c.end.Sub(c.start))
	best := map[string][2]int{} // per local day: the longest run of sunny hours, as indexes
	runStart := -1
	flush := func(i int) {
		if runStart < 0 {
			return
		}
		day := c.skyHours[runStart].t.In(loc).Format("2006-01-02")
		if r, ok := best[day]; !ok || i-runStart > r[1]-r[0] {
			best[day] = [2]int{runStart, i}
		}
		runStart = -1
	}
	for i, h := range c.skyHours {
		mid := h.t.Add(30 * time.Minute)
		sunny := h.cover < clearSkyCover && sunElevation(lat, lon, mid) > 0
		if i > 0 && !sameDay(c.skyHours[i-1].t.In(loc), h.t.In(loc)) {
			flush(i)
		}
		if sunny && runStart < 0 {
			runStart = i
		} else if !sunny {
			flush(i)
		}
	}
	flush(len(c.skyHours))
	for _, r := range best {
		if r[1]-r[0] < minSunnyHours {
			continue
		}
		from, to := c.skyHours[r[0]].t, c.skyHours[r[1]-1].t.Add(time.Hour)
		mid := from.Add(to.Sub(from) / 2)
		c.Suns = append(c.Suns, math.Round(float64(mid.Sub(c.start))/span*1000)/10)
	}
	sort.Float64s(c.Suns)
}

// sunElevation is the sun's approximate height above the horizon, in degrees (within about a
// degree, which is plenty to tell day from night).
func sunElevation(lat, lon float64, t time.Time) float64 {
	rad := math.Pi / 180
	d := t.UTC().Sub(time.Date(2000, 1, 1, 12, 0, 0, 0, time.UTC)).Hours() / 24
	g := (357.529 + 0.98560028*d) * rad
	q := 280.459 + 0.98564736*d
	l := (q + 1.915*math.Sin(g) + 0.020*math.Sin(2*g)) * rad
	e := (23.439 - 0.00000036*d) * rad
	ra := math.Atan2(math.Cos(e)*math.Sin(l), math.Cos(l))
	dec := math.Asin(math.Sin(e) * math.Sin(l))
	gmst := math.Mod(18.697374558+24.06570982441908*d, 24)
	h := gmst*15*rad + lon*rad - ra
	return math.Asin(math.Sin(lat*rad)*math.Sin(dec)+math.Cos(lat*rad)*math.Cos(dec)*math.Cos(h)) / rad
}

// Mark is something placed in the chart's top band at X (0-100 %): a sun over a sunny stretch, or a
// windy spell's peak gust.
type Mark struct {
	X     float64 `json:"x"`
	Kind  string  `json:"kind"`            // "sun" or "gust"
	Label string  `json:"label,omitempty"` // the gust
}

// setMarks gathers the suns and gust labels into Marks, left to right.
func (c *Chart) setMarks() {
	c.Marks = []Mark{}
	for _, x := range c.Suns {
		c.Marks = append(c.Marks, Mark{X: x, Kind: "sun"})
	}
	for _, g := range c.Gusts {
		c.Marks = append(c.Marks, Mark{X: g.X, Kind: "gust", Label: g.Labels[0]})
	}
	sort.SliceStable(c.Marks, func(i, j int) bool { return c.Marks[i].X < c.Marks[j].X })
}
