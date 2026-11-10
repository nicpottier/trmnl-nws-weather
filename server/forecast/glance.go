package forecast

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"
	_ "time/tzdata" // Lambda's provided.al2023 runtime ships no zoneinfo
)

// Glance is the compact payload TRMNL polls; everything the Liquid templates need, nothing more.
type Glance struct {
	Location    string  `json:"location"`
	Unit        string  `json:"unit"`
	Updated     string  `json:"updated"`
	Today       string  `json:"today"`
	Now         Now     `json:"now"`
	Outlook     string  `json:"outlook"`
	OutlookName string  `json:"outlook_name"`
	Days        []Day   `json:"days"`
	TodayIndex  int     `json:"today_index"` // days[today_index] is today; earlier days are measured
	FreshSnow   string  `json:"fresh_snow"`  // measured new snow, e.g. "3″ 24h · 7″ 48h" or "7″ 48h", or ""
	Alerts      []Alert `json:"alerts"`
	Scale       Scale   `json:"scale"`
	Chart       *Chart  `json:"chart,omitempty"`
}

type Now struct {
	Temp       int     `json:"temp"`
	Source     string  `json:"source"`      // observing station ID, or "forecast" if none is fresh
	ObservedAt string  `json:"observed_at"` // local time of the observation
	Short      string  `json:"short"`
	Icon       string  `json:"icon"`
	Pop        int     `json:"pop"`
	Humidity   int     `json:"humidity"`
	Wind       string  `json:"wind"`
	Trend      string  `json:"trend"` // "up", "down" or "flat" over the next 3 hours
	Pct        float64 `json:"pct"`   // position on the shared temperature scale, 0 = warmest (top)
}

type Day struct {
	Dow       string   `json:"dow"`
	Date      int      `json:"date"`
	Label     string   `json:"label"`
	Name      string   `json:"name"`
	Hi        int      `json:"hi"`
	Lo        int      `json:"lo"`
	Pop       int      `json:"pop"`
	Short     string   `json:"short"`
	Icon      string   `json:"icon"`
	Severity  int      `json:"severity"`
	Shade     string   `json:"shade"`
	TopPct    float64  `json:"top_pct"`
	HeightPct float64  `json:"height_pct"`
	Snow      string   `json:"snow"`    // forecast snowfall for the day, e.g. "0.8″", or ""
	Rain      string   `json:"rain"`    // forecast rain for the day, e.g. "0.13″", or ""
	Amounts   []string `json:"amounts"` // the day's amounts in words, e.g. ["1.2″ snow", "Sprinkles"]
	Gust      int      `json:"gust"`    // the day's peak gust (mph or km/h) when windy, else 0
}

type Scale struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

type Options struct {
	Lat, Lon float64
	Units    string // "us" or "si"
	Name     string // overrides the NWS city name
	Days     int
	Bars     string // chart bars: "auto", "snow" or "precip"
}

func BuildGlance(ctx context.Context, nws *nwsClient, opts Options, now time.Time) (*Glance, error) {
	points, err := nws.Points(ctx, opts.Lat, opts.Lon)
	if err != nil {
		return nil, fmt.Errorf("looking up NWS grid: %w", err)
	}
	pp := points.Properties

	var (
		wg                 sync.WaitGroup
		periods, hourly    []Period
		alerts             []Alert
		grid               *GridData
		periodsErr, hrsErr error
		stations, snotel   []stationSeries
	)
	// Station readings since yesterday's local midnight (plus an hour, for the first hourly
	// average), and at least the 48 hours the fresh-snow total covers.
	since := now.Add(-49 * time.Hour)
	if tz, err := time.LoadLocation(pp.TimeZone); err == nil {
		l := now.In(tz)
		since = minTime(since, time.Date(l.Year(), l.Month(), l.Day()-1, 0, 0, 0, 0, tz).Add(-time.Hour))
	}
	wg.Add(6)
	go func() { defer wg.Done(); periods, periodsErr = nws.Forecast(ctx, pp.Forecast, opts.Units) }()
	go func() { defer wg.Done(); hourly, hrsErr = nws.Forecast(ctx, pp.ForecastHourly, opts.Units) }()
	go func() { defer wg.Done(); alerts, _ = nws.Alerts(ctx, opts.Lat, opts.Lon) }()
	go func() { defer wg.Done(); grid, _ = nws.GridData(ctx, pp.ForecastGridData) }() // chart is optional
	go func() {
		defer wg.Done()
		stations, _ = nws.stationSeriesNear(ctx, opts.Lat, opts.Lon, pp.RelativeLocation.Properties.State, since)
	}()
	go func() {
		defer wg.Done()
		snotel, _ = nws.snotelSeriesNear(ctx, opts.Lat, opts.Lon, pp.RelativeLocation.Properties.State, since, now)
	}()
	wg.Wait()

	if periodsErr != nil {
		return nil, fmt.Errorf("fetching forecast: %w", periodsErr)
	}
	if len(periods) == 0 {
		return nil, fmt.Errorf("forecast has no periods")
	}
	if hrsErr != nil {
		hourly = nil // degrade: current conditions come from the 12-hour forecast instead
	}
	if alerts == nil {
		alerts = []Alert{}
	}

	loc, err := time.LoadLocation(pp.TimeZone)
	if err != nil {
		loc = periods[0].StartTime.Location()
	}
	localNow := now.In(loc)
	if err := nws.addCWOPRain(ctx, stations, loc, now); err != nil { // rain from home weather stations (aprs.go)
		log.Printf("cwop rain for %.4f,%.4f: %v", opts.Lat, opts.Lon, err)
	}

	hour := currentHour(hourly, now)
	if hour == nil {
		hour = &periods[0]
	}

	// Prefer a measured temperature; the forecast for this hour can be several degrees off.
	nowTemp, humidity, source, observedAt := hour.Temperature, hour.RelativeHumidity.Or(0), "forecast", ""
	pointElevM := math.NaN()
	if grid != nil && grid.Properties.Elevation.Value != nil {
		pointElevM = *grid.Properties.Elevation.Value
	}
	station := pickStation(stations, pointElevM, now)
	obs, err := (*Observation)(nil), error(nil)
	if station != nil {
		obs = station.latest()
	} else {
		obs, err = nws.CurrentObservation(ctx, opts.Lat, opts.Lon, points, pointElevM, loc, now)
	}
	if err == nil && obs != nil {
		nowTemp, source, observedAt = fToUnits(obs.TempF, opts.Units), obs.Station, obs.ObservedAt.In(loc).Format("3:04 PM")
		if obs.Humidity != nil {
			humidity = *obs.Humidity
		}
	}

	// With a day of station readings, the chart starts yesterday, drawn from what was measured,
	// and shows one forecast day fewer.
	back := 0
	days := []Day{}
	midnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, loc)
	if station != nil && opts.Days > 1 {
		if d, ok := observedDay(station, midnight.AddDate(0, 0, -1), opts.Units); ok {
			days, back = append(days, d), 1
		}
	}
	forecastDays := buildDays(periods, hourly, loc, opts.Days-back)
	if len(forecastDays) == 0 {
		days, back = forecastDays, 0
	}
	days = append(days, forecastDays...)
	if station != nil && len(days) > back {
		includeObserved(&days[back], station, localNow, opts.Units)
	}
	// Measured snow and rain come from SNOTEL (api.weather.gov's stations have neither); wind from both.
	seen := newObserved(append(append([]stationSeries{}, stations...), snotel...), pointElevM)

	nowIcon, _ := classify(hour.ShortForecast, hour.Pop())
	if !hour.IsDaytime {
		switch nowIcon {
		case "clear":
			nowIcon = "night"
		case "partly":
			nowIcon = "night_partly"
		}
	}

	// One scale shared by every pill and the "now" marker.
	lo, hi := nowTemp, nowTemp
	for _, d := range days {
		lo, hi = min(lo, d.Lo), max(hi, d.Hi)
	}
	span := float64(max(hi-lo, 1))
	pct := func(t int) float64 { return math.Round(float64(hi-t)*1000/span) / 10 }
	for i := range days {
		days[i].TopPct = pct(days[i].Hi)
		days[i].HeightPct = math.Round(float64(days[i].Hi-days[i].Lo)*1000/span) / 10
	}

	var chart *Chart
	if grid != nil && len(days) > 0 {
		chart = buildChart(grid, days, loc, now, opts.Units, opts.Bars, station, seen, back)
		nowIcon = currentIcon(nowIcon, hour.ShortForecast, chart, nowTemp, opts.Units)
	}

	name := opts.Name
	if name == "" {
		rel := pp.RelativeLocation.Properties
		name = strings.Trim(rel.City+", "+rel.State, ", ")
	}
	unit := "F"
	if opts.Units == "si" {
		unit = "C"
	}
	wind := strings.TrimSpace(hour.WindDirection + " " + hour.WindSpeed)
	if strings.HasPrefix(hour.WindSpeed, "0 ") {
		wind = "Calm"
	}

	return &Glance{
		Location: name,
		Unit:     unit,
		Updated:  localNow.Format("3:04 PM"),
		Today:    localNow.Format("Mon Jan 2"),
		Now: Now{
			Temp:       nowTemp,
			Source:     source,
			ObservedAt: observedAt,
			Short:      hour.ShortForecast,
			Icon:       nowIcon,
			Pop:        hour.Pop(),
			Humidity:   int(math.Round(humidity)),
			Wind:       wind,
			Trend:      trend(hourly, hour, nowTemp),
			Pct:        pct(nowTemp),
		},
		Outlook:     periods[0].DetailedForecast,
		OutlookName: periods[0].Name,
		Days:        days,
		TodayIndex:  back,
		FreshSnow:   freshSnow(seen, now, opts.Units),
		Alerts:      alerts[:min(len(alerts), 3)],
		Scale:       Scale{Min: lo, Max: hi},
		Chart:       chart,
	}, nil
}

// currentIcon refines the icon for the current hour (from its NWS wording) with the rain and
// snow the grid forecasts for the rest of today, so the icon matches the amounts on the chart.
//
//   - Measurable amounts decide: both snow and rain past the light limits is a rain/snow mix
//     ("sleet"), otherwise snow, rain, or flurries / sprinkles when light. This also catches
//     rain or snow later today under a sky that's only cloudy right now.
//   - Wording that mentions precipitation without a measurable amount ("Chance Rain And Snow"
//     with nothing in the grid) is flurries or sprinkles; for a rain/snow mix the temperature
//     picks (34°F / 1°C or colder is flurries).
//   - Thunderstorms keep their icon.
func currentIcon(icon, short string, c *Chart, temp int, units string) string {
	if icon == "storm" {
		return icon
	}
	heavySnow, heavyRain := c.restSnow >= c.flurries, c.restRain >= c.sprinkles
	switch {
	case heavySnow && heavyRain:
		return "sleet"
	case heavySnow:
		return "snow"
	case heavyRain:
		return "rain"
	case c.restSnow > 0:
		return "flurries"
	case c.restRain > 0:
		return "sprinkles"
	}

	switch icon {
	case "rain", "sprinkles":
		return "sprinkles"
	case "snow", "flurries":
		text := strings.ToLower(short)
		if strings.Contains(text, "rain") || strings.Contains(text, "drizzle") || strings.Contains(text, "shower") {
			cold := 34
			if units == "si" {
				cold = 1
			}
			if temp > cold {
				return "sprinkles"
			}
		}
		return "flurries"
	}
	return icon // sky (clear, partly, cloudy, ...) or a freezing mix, with nothing measurable today
}

// includeObserved widens today's low and high to cover what a station has measured since
// midnight: late in the day the forecast only covers what's left, so its "high" can be well
// below the afternoon that already happened.
func includeObserved(today *Day, s *stationSeries, localNow time.Time, units string) {
	midnight := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, localNow.Location())
	for i, t := range s.Times {
		if t.Before(midnight) {
			continue
		}
		v := fToUnits(s.TempF[i], units)
		today.Hi, today.Lo = max(today.Hi, v), min(today.Lo, v)
	}
}

// minObservedDayHours is how many hours of a past day a station must have reported for the day to
// be drawn from its readings. api.weather.gov's copy of LVN11 skips a few hours most days.
const minObservedDayHours = 12

// observedDay builds a past day's entry from a station's readings, if they cover enough of it.
func observedDay(s *stationSeries, from time.Time, units string) (Day, bool) {
	to := from.AddDate(0, 0, 1)
	hours := map[int64]bool{}
	hi, lo := math.MinInt, math.MaxInt
	for i, t := range s.Times {
		if t.Before(from) || !t.Before(to) {
			continue
		}
		hours[t.Truncate(time.Hour).Unix()] = true
		v := fToUnits(s.TempF[i], units)
		hi, lo = max(hi, v), min(lo, v)
	}
	if len(hours) < minObservedDayHours {
		return Day{}, false
	}
	return Day{
		Dow:   from.Format("Mon"),
		Date:  from.Day(),
		Label: from.Format("Mon 2"),
		Name:  "Yesterday",
		Hi:    hi,
		Lo:    lo,
		Shade: shades[0],
	}, true
}

// freshSnow describes the snow measured in the last 24 and 48 hours, or "" when none fell.
func freshSnow(o *observed, now time.Time, units string) string {
	if o == nil || len(o.depths) == 0 {
		return ""
	}
	d1 := o.snow(now.Add(-24*time.Hour), now, freshSnowMinRise)
	d2 := math.Max(d1, o.snow(now.Add(-48*time.Hour), now, freshSnowMinRise))
	if d2 == 0 {
		return ""
	}
	f := func(in float64) string {
		if units == "si" {
			return fmt.Sprintf("%.0fcm", in*2.54)
		}
		return fmt.Sprintf("%.0f″", in)
	}
	// The 24-hour total only when some fell in the last day (and it rounds to something).
	if f(d1) == f(0) {
		return f(d2) + " 48h"
	}
	return f(d1) + " 24h · " + f(d2) + " 48h"
}

func trend(hourly []Period, hour *Period, nowTemp int) string {
	for _, h := range hourly {
		if !h.StartTime.Before(hour.StartTime.Add(3 * time.Hour)) {
			switch {
			case h.Temperature > nowTemp:
				return "up"
			case h.Temperature < nowTemp:
				return "down"
			}
			break
		}
	}
	return "flat"
}

func currentHour(hourly []Period, now time.Time) *Period {
	for i := range hourly {
		if hourly[i].EndTime.After(now) {
			return &hourly[i]
		}
	}
	return nil
}

func sameDay(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

// buildDays pairs each daytime period with the night after it to get the day's high and low.
func buildDays(periods, hourly []Period, loc *time.Location, limit int) (days []Day) {
	days = []Day{}
	i := 0

	// Between midnight and 6 AM NWS starts with "Overnight", a night period on the same date as the
	// "Today" after it. It isn't a day of its own: its low folds into today's.
	overnight := false
	if !periods[0].IsDaytime && len(periods) > 1 && sameDay(periods[0].StartTime.In(loc), periods[1].StartTime.In(loc)) {
		overnight = true
		defer func() {
			if len(days) > 0 {
				days[0].Lo = min(days[0].Lo, periods[0].Temperature)
			}
		}()
	}

	// Late in the day NWS starts with "Tonight": today's high is the warmest hour left today.
	if overnight {
		i = 1
	} else if !periods[0].IsDaytime {
		night := periods[0]
		start := night.StartTime.In(loc)
		hi := night.Temperature
		for _, h := range hourly {
			if sameDay(h.StartTime.In(loc), start) {
				hi = max(hi, h.Temperature)
			}
		}
		days = append(days, dayEntry(start, hi, night.Temperature, night, night))
		i = 1
	}

	for ; i < len(periods) && len(days) < limit; i += 2 {
		day := periods[i]
		start := day.StartTime.In(loc)
		if i+1 < len(periods) && !periods[i+1].IsDaytime {
			night := periods[i+1]
			days = append(days, dayEntry(start, day.Temperature, night.Temperature, day, night))
			continue
		}
		// The final day has no night period; use the hourly low if we have one.
		lo, found := 0, false
		for _, h := range hourly {
			if sameDay(h.StartTime.In(loc), start) {
				lo, found = h.Temperature, true
				break
			}
		}
		if !found {
			break
		}
		for _, h := range hourly {
			if sameDay(h.StartTime.In(loc), start) {
				lo = min(lo, h.Temperature)
			}
		}
		days = append(days, dayEntry(start, day.Temperature, lo, day, day))
	}
	return days
}

// Shades for severity 0 (fair) .. 5 (dangerous); on 1-bit screens these render as denser dithers.
var shades = []string{"gray-75", "gray-70", "gray-65", "gray-60", "gray-55", "gray-45"}

func dayEntry(start time.Time, hi, lo int, day, night Period) Day {
	icon, severity := classify(day.ShortForecast, day.Pop())
	_, nightSeverity := classify(night.ShortForecast, night.Pop())
	severity = max(severity, nightSeverity-1)
	return Day{
		Dow:      start.Format("Mon"),
		Date:     start.Day(),
		Label:    start.Format("Mon 2"),
		Name:     day.Name,
		Hi:       max(hi, lo),
		Lo:       min(hi, lo),
		Pop:      max(day.Pop(), night.Pop()),
		Short:    day.ShortForecast,
		Icon:     icon,
		Severity: severity,
		Shade:    shades[severity],
	}
}

type condition struct {
	words    []string
	icon     string
	severity int
}

// Ordered most to least severe; the first match in the leading clause picks the icon.
var conditions = []condition{
	{[]string{"thunder", "t-storm", "tstorm"}, "storm", 5},
	{[]string{"blizzard", "heavy snow", "freezing", "ice", "sleet"}, "sleet", 5},
	{[]string{"flurr", "light snow"}, "flurries", 3},
	{[]string{"snow"}, "snow", 4},
	{[]string{"drizzle", "sprinkle", "light rain"}, "sprinkles", 2},
	{[]string{"rain", "shower"}, "rain", 3},
	{[]string{"fog", "haze", "smoke", "dust"}, "fog", 2},
	{[]string{"partly", "mostly sunny", "mostly clear", "few clouds"}, "partly", 1},
	{[]string{"mostly cloudy", "cloudy", "overcast"}, "cloudy", 2},
	{[]string{"wind", "breezy", "blustery"}, "wind", 1},
	{[]string{"sunny", "clear", "fair", "hot"}, "clear", 0},
}

// classify maps an NWS short forecast ("Rain Showers Likely then Chance Rain And Snow Showers")
// to an icon name and a 0-5 severity used to shade the day's pill.
func classify(short string, pop int) (string, int) {
	text := strings.ToLower(short)
	first, _, _ := strings.Cut(text, " then ")
	matches := func(c condition, s string) bool {
		for _, w := range c.words {
			if strings.Contains(s, w) {
				return true
			}
		}
		return false
	}

	icon, severity := "cloudy", 2.0
	for _, c := range conditions {
		if matches(c, first) {
			icon, severity = c.icon, float64(c.severity)
			break
		}
	}
	// A worse condition later in the day still darkens the pill, a notch less than if it led.
	for _, c := range conditions {
		if matches(c, text) && c.severity >= 3 {
			severity = max(severity, float64(c.severity-1))
		}
	}
	switch {
	case strings.Contains(first, "slight chance") || strings.Contains(first, "isolated"):
		severity--
	case strings.Contains(first, "chance") || strings.Contains(first, "scattered"):
		severity -= 0.5
	}
	if pop >= 70 {
		severity += 0.5
	}
	return icon, int(math.Max(0, math.Min(5, math.Round(severity))))
}
