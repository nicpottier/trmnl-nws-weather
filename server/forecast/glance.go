package forecast

import (
	"context"
	"fmt"
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
	Amounts   []string `json:"amounts"` // what the chart lists under the day, e.g. ["0.8″ snow", "0.13″ rain"]
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
	)
	wg.Add(4)
	go func() { defer wg.Done(); periods, periodsErr = nws.Forecast(ctx, pp.Forecast, opts.Units) }()
	go func() { defer wg.Done(); hourly, hrsErr = nws.Forecast(ctx, pp.ForecastHourly, opts.Units) }()
	go func() { defer wg.Done(); alerts, _ = nws.Alerts(ctx, opts.Lat, opts.Lon) }()
	go func() { defer wg.Done(); grid, _ = nws.GridData(ctx, pp.ForecastGridData) }() // chart is optional
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
	if obs, err := nws.CurrentObservation(ctx, opts.Lat, opts.Lon, points, pointElevM, loc, now); err == nil {
		nowTemp, source, observedAt = fToUnits(obs.TempF, opts.Units), obs.Station, obs.ObservedAt.In(loc).Format("3:04 PM")
		if obs.Humidity != nil {
			humidity = *obs.Humidity
		}
	}

	days := buildDays(periods, hourly, loc, opts.Days)

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
		chart = buildChart(grid, days, loc, now, opts.Units, opts.Bars)
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
		Alerts:      alerts[:min(len(alerts), 3)],
		Scale:       Scale{Min: lo, Max: hi},
		Chart:       chart,
	}, nil
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
func buildDays(periods, hourly []Period, loc *time.Location, limit int) []Day {
	days := []Day{}
	i := 0

	// Late in the day NWS starts with "Tonight": today's high is the warmest hour left today.
	if !periods[0].IsDaytime {
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
