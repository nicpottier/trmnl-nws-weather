package forecast

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GridData is the subset of /gridpoints/{wfo}/{x},{y} the chart needs. Every series is a list
// of values over ISO 8601 intervals ("2026-10-25T12:00:00+00:00/PT6H"), in metric units.
type GridData struct {
	Properties struct {
		Elevation                 Quantity   `json:"elevation"`
		Temperature               GridSeries `json:"temperature"`
		SnowfallAmount            GridSeries `json:"snowfallAmount"`
		QuantitativePrecipitation GridSeries `json:"quantitativePrecipitation"`
	} `json:"properties"`
}

type GridSeries struct {
	Values []struct {
		ValidTime string   `json:"validTime"`
		Value     *float64 `json:"value"`
	} `json:"values"`
}

func (c *nwsClient) GridData(ctx context.Context, url string) (*GridData, error) {
	g := &GridData{}
	if err := c.get(ctx, url, g); err != nil {
		return nil, err
	}
	return g, nil
}

var durationRE = regexp.MustCompile(`^P(?:(\d+)D)?(?:T(?:(\d+)H)?(?:(\d+)M)?)?$`)

// parseInterval parses "2026-10-25T12:00:00+00:00/PT6H" (NWS only uses days, hours and minutes).
func parseInterval(s string) (time.Time, time.Time, error) {
	startS, durS, ok := strings.Cut(s, "/")
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("bad interval %q", s)
	}
	start, err := time.Parse(time.RFC3339, startS)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	m := durationRE.FindStringSubmatch(durS)
	if m == nil {
		return time.Time{}, time.Time{}, fmt.Errorf("bad duration %q", durS)
	}
	var d time.Duration
	for i, unit := range []time.Duration{24 * time.Hour, time.Hour, time.Minute} {
		if m[i+1] != "" {
			n, _ := strconv.Atoi(m[i+1])
			d += time.Duration(n) * unit
		}
	}
	return start, start.Add(d), nil
}

type interval struct {
	start, end time.Time
	value      float64
}

func (s GridSeries) intervals() []interval {
	out := []interval{}
	for _, v := range s.Values {
		if v.Value == nil {
			continue
		}
		start, end, err := parseInterval(v.ValidTime)
		if err != nil {
			continue
		}
		out = append(out, interval{start, end, *v.Value})
	}
	return out
}

// amountBetween sums an accumulation series over [from, to), splitting intervals proportionally.
func amountBetween(ivs []interval, from, to time.Time) float64 {
	total := 0.0
	for _, iv := range ivs {
		lo, hi := maxTime(iv.start, from), minTime(iv.end, to)
		if !hi.After(lo) {
			continue
		}
		total += iv.value * float64(hi.Sub(lo)) / float64(iv.end.Sub(iv.start))
	}
	return total
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Chart is drawn in two layers: an SVG temperature line in a 1000x100 viewBox stretched over the
// chart area, and HTML bars (so the Framework can dither their shading on 1-bit screens).
type Chart struct {
	TempPath  string   `json:"temp_path"`    // SVG path, viewBox 0 0 1000 100
	FreezeY   *float64 `json:"freeze_y"`     // y of 32°F / 0°C in the viewBox, if in range
	NowX      float64  `json:"now_x"`        // x of the current time, 0-1000
	NowPct    float64  `json:"now_pct"`      // the same, as a percentage
	NowY      *float64 `json:"now_y"`        // y of the temperature line at now_x (0-100), if known
	TempMax   int      `json:"temp_max"`     // temperature at the top of the chart
	TempMin   int      `json:"temp_min"`     // temperature at the bottom
	BarKind   string   `json:"bar_kind"`     // "auto", "snow" or "precip"
	Bars      []Bar    `json:"bars"`         // one per 6 hours
	HasAmount bool     `json:"has_amount"`   // any snow/rain at all in the window
	Days      int      `json:"days"`         // columns in the chart
	Snow      float64  `json:"snow_total"`   // total snowfall over the window, display units
	Rain      float64  `json:"rain_total"`   // total rain (or all precip, in "precip" mode)
	Label     string   `json:"label"`        // e.g. "Snow 1.8″ · Rain 0.25″"
	Freeze    string   `json:"freeze_label"` // "32°" or "0°"

	samples []sample // hourly temperatures in display units, kept so the scale can be changed
	freeze  float64  // 32 or 0, in display units
}

type sample struct {
	x, t float64 // x in the 0-1000 viewBox, temperature in display units
}

const barHours = 6

// buildChart lays out days*24 hours starting at local midnight today, and fills each Day's
// snow/precip totals. barMode is "auto", "snow" or "precip".
func buildChart(grid *GridData, days []Day, loc *time.Location, now time.Time, units, barMode string) *Chart {
	n := len(days)
	local := now.In(loc)
	start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	end := start.AddDate(0, 0, n)
	span := float64(end.Sub(start))
	xOf := func(t time.Time) float64 { return float64(t.Sub(start)) / span * 1000 }

	c := &Chart{Days: n, Freeze: "32°"}
	c.NowX = math.Round(xOf(now)*10) / 10
	c.NowPct = c.NowX / 10

	// Hourly temperature samples, converted to display units.
	for _, iv := range grid.Properties.Temperature.intervals() {
		for t := iv.start; t.Before(iv.end); t = t.Add(time.Hour) {
			if t.Before(start) || !t.Before(end) {
				continue
			}
			temp := iv.value
			if units != "si" {
				temp = temp*9/5 + 32
			}
			c.samples = append(c.samples, sample{xOf(t), temp})
		}
	}
	c.freeze = 32
	if units == "si" {
		c.freeze, c.Freeze = 0, "0°"
	}
	if lo, hi, ok := c.tempRange(); ok {
		c.setScale(lo, hi)
	}

	c.fillBars(grid, days, start, end, units, barMode)
	return c
}

// Bar is one 6-hour bucket of the chart.
type Bar struct {
	Height float64 `json:"h"`     // 0-100 % of the chart height, against its kind's scale
	Kind   string  `json:"kind"`  // "snow", "rain", or "" for none
	Shade  string  `json:"shade"` // Framework bg-- shade
}

var barShades = map[string]string{"snow": "gray-35", "rain": "gray-55"}

// Bar scales, per 6 hours. They never change, so the bars read the same on every screen and
// every day: a half-height bar is 6″ of snow (0.6″ of rain), height is proportional to the
// amount, and anything past full height (12″ / 1.2″) is drawn at full height, with the day's
// amount label giving the real number. Snow and rain use the usual 10:1 snow-to-water ratio,
// so equal heights hold about the same amount of water.
const (
	snowHalfIn, rainHalfIn = 6.0, 0.6
	snowHalfCm, rainHalfMm = 15.0, 15.0
)

// barHeight maps an amount to 0-100 % of the chart height, with half the height at half.
func barHeight(amount, half float64) float64 {
	return math.Min(100, math.Round(500*amount/half)/10)
}

// fillBars buckets snowfall and precipitation into 6-hour bars and per-day totals.
//
// In "auto" mode a bucket with any snow is a snow bar (in inches of snow) and any other wet
// bucket is a rain bar (inches of liquid), each on its fixed scale, so a mountain pass
// shows both its rain days and its snow days. "snow" shows only snowfall, and "precip" shows
// all liquid precipitation (snow included, as water) as rain bars.
func (c *Chart) fillBars(grid *GridData, days []Day, start, end time.Time, units, mode string) {
	snowIvs := grid.Properties.SnowfallAmount.intervals()
	precipIvs := grid.Properties.QuantitativePrecipitation.intervals()
	snowConv, rainConv := 1/25.4, 1/25.4 // mm -> in
	snowHalf, rainHalf := snowHalfIn, rainHalfIn
	if units == "si" {
		snowConv, rainConv = 0.1, 1 // mm -> cm, mm
		snowHalf, rainHalf = snowHalfCm, rainHalfMm
	}
	if mode != "snow" && mode != "precip" {
		mode = "auto"
	}
	c.BarKind = mode

	type bucket struct{ snow, rain float64 }
	buckets := []bucket{}
	anyAmount := false
	for t := start; t.Before(end); t = t.Add(barHours * time.Hour) {
		next := t.Add(barHours * time.Hour)
		sn := amountBetween(snowIvs, t, next) * snowConv
		pr := amountBetween(precipIvs, t, next) * rainConv
		b := bucket{}
		switch {
		case mode == "precip":
			b.rain = pr
		case mode == "snow":
			b.snow = sn
		case sn >= 0.01: // auto: any measurable snow makes it a snow bucket
			b.snow = sn
		default:
			b.rain = pr
		}
		buckets = append(buckets, b)
		anyAmount = anyAmount || b.snow > 0 || b.rain > 0
	}

	for _, b := range buckets {
		bar := Bar{}
		switch {
		case b.snow > 0:
			bar = Bar{barHeight(b.snow, snowHalf), "snow", barShades["snow"]}
		case b.rain > 0:
			bar = Bar{barHeight(b.rain, rainHalf), "rain", barShades["rain"]}
		}
		c.Bars = append(c.Bars, bar)
		c.Snow += b.snow
		c.Rain += b.rain
	}
	c.HasAmount = anyAmount

	rainWord := "rain"
	if mode == "precip" {
		rainWord = "precip"
	}
	perDay := 24 / barHours
	for i := range days {
		sn, rn := 0.0, 0.0
		for _, b := range buckets[min(i*perDay, len(buckets)):min((i+1)*perDay, len(buckets))] {
			sn, rn = sn+b.snow, rn+b.rain
		}
		days[i].Snow = formatAmount(sn, "snow", units)
		days[i].Rain = formatAmount(rn, "rain", units)
		days[i].Amounts = []string{}
		if days[i].Snow != "" {
			days[i].Amounts = append(days[i].Amounts, days[i].Snow+" snow")
		}
		if days[i].Rain != "" {
			days[i].Amounts = append(days[i].Amounts, days[i].Rain+" "+rainWord)
		}
	}

	parts := []string{}
	if s := formatAmount(c.Snow, "snow", units); s != "" {
		parts = append(parts, "Snow "+s)
	}
	if r := formatAmount(c.Rain, "rain", units); r != "" {
		parts = append(parts, strings.ToUpper(rainWord[:1])+rainWord[1:]+" "+r)
	}
	c.Label = strings.Join(parts, " · ")
	if c.Label == "" {
		c.Label = "No precipitation"
	}
}

// tempRange returns the chart's temperature extremes, rounded out to a multiple of 5 so the
// line never touches the top or bottom edge.
func (c *Chart) tempRange() (int, int, bool) {
	if len(c.samples) < 2 {
		return 0, 0, false
	}
	lo, hi := c.samples[0].t, c.samples[0].t
	for _, s := range c.samples {
		lo, hi = math.Min(lo, s.t), math.Max(hi, s.t)
	}
	return int(math.Floor((lo-1)/5) * 5), int(math.Ceil((hi+1)/5) * 5), true
}

// setScale (re)draws the temperature line and freezing line for a chart spanning lo..hi.
func (c *Chart) setScale(lo, hi int) {
	c.TempMin, c.TempMax = lo, hi
	yOf := func(t float64) float64 {
		return math.Round((1-(t-float64(lo))/float64(hi-lo))*1000) / 10
	}
	var b strings.Builder
	for i, s := range c.samples {
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&b, "%s%.1f,%.1f", cmd, s.x, yOf(s.t))
	}
	c.TempPath = b.String()

	// Where the line is at the current time, interpolating between the hourly samples around it.
	c.NowY = nil
	for i := 1; i < len(c.samples); i++ {
		a, z := c.samples[i-1], c.samples[i]
		if a.x <= c.NowX && c.NowX <= z.x && z.x > a.x {
			y := yOf(a.t + (z.t-a.t)*(c.NowX-a.x)/(z.x-a.x))
			c.NowY = &y
			break
		}
	}

	c.FreezeY = nil
	if c.freeze > float64(lo) && c.freeze < float64(hi) {
		y := yOf(c.freeze)
		c.FreezeY = &y
	}
}

// ShareScale puts every chart on one temperature scale, wide enough for all of them, so
// stacked locations can be compared at a glance.
func ShareScale(charts []*Chart) {
	lo, hi, found := 0, 0, false
	for _, c := range charts {
		if c == nil {
			continue
		}
		if l, h, ok := c.tempRange(); ok {
			if !found {
				lo, hi, found = l, h, true
			}
			lo, hi = min(lo, l), max(hi, h)
		}
	}
	for _, c := range charts {
		if c != nil && len(c.samples) > 1 {
			c.setScale(lo, hi)
		}
	}
}

// formatAmount gives a short display amount for a day, or "" when it's effectively zero.
func formatAmount(v float64, kind, units string) string {
	switch {
	case kind == "snow" && units != "si" && v < 0.05,
		kind == "snow" && units == "si" && v < 0.1,
		kind == "rain" && units != "si" && v < 0.005,
		kind == "rain" && units == "si" && v < 0.1:
		return ""
	}
	unit := "″"
	if units == "si" {
		unit = "cm"
		if kind == "rain" {
			unit = "mm"
		}
	}
	return trimAmount(v, kind, units) + unit
}

func trimAmount(v float64, kind, units string) string {
	if kind == "rain" && units != "si" {
		return strconv.FormatFloat(math.Round(v*100)/100, 'f', 2, 64)
	}
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', 1, 64)
}
