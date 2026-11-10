package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		short    string
		pop      int
		icon     string
		severity int
	}{
		{"Sunny", 0, "clear", 0},
		{"Mostly Sunny", 10, "partly", 1},
		{"Partly Sunny", 12, "partly", 1},
		{"Mostly Cloudy", 12, "cloudy", 2},
		{"Chance Rain Showers", 44, "rain", 3},
		{"Slight Chance Rain And Snow Showers", 22, "snow", 3},
		{"Rain Showers Likely", 58, "rain", 3},
		{"Rain And Snow Showers Likely", 65, "snow", 4},
		{"Rain Showers Likely then Chance Rain And Snow Showers", 60, "rain", 3},
		{"Partly Cloudy then Slight Chance Rain Showers", 17, "partly", 2},
		{"Showers And Thunderstorms Likely", 70, "storm", 5},
		{"Freezing Rain", 80, "sleet", 5},
		{"Patchy Fog", 0, "fog", 2},
		{"Flurries", 20, "flurries", 3},
		{"Chance Light Snow", 40, "flurries", 3},
		{"Light Snow Likely", 60, "flurries", 3},
		{"Patchy Drizzle", 20, "sprinkles", 2},
		{"Light Rain", 50, "sprinkles", 2},
	}
	for _, tc := range tests {
		icon, severity := classify(tc.short, tc.pop)
		if icon != tc.icon || severity != tc.severity {
			t.Errorf("classify(%q, %d) = %s, %d; want %s, %d", tc.short, tc.pop, icon, severity, tc.icon, tc.severity)
		}
	}
}

// fakeNWS serves the recorded Leavenworth responses in testdata/, optionally dropping
// leading forecast periods to simulate fetching later in the day. Without withStations the state
// has no stations, so current conditions fall back to forecast.weather.gov and the API's latest.
func fakeNWS(t *testing.T, dropPeriods int, withStations bool) *httptest.Server {
	t.Helper()
	read := func(name string) map[string]any {
		b, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		switch {
		case strings.HasPrefix(r.URL.Path, "/points/"):
			v = read("points.json")
			props := v["properties"].(map[string]any)
			props["forecast"] = srv.URL + "/forecast"
			props["forecastHourly"] = srv.URL + "/forecast/hourly"
			props["observationStations"] = srv.URL + "/stations"
			props["forecastGridData"] = srv.URL + "/gridpoints"
		case r.URL.Path == "/forecast":
			v = read("forecast.json")
			props := v["properties"].(map[string]any)
			props["periods"] = props["periods"].([]any)[dropPeriods:]
		case r.URL.Path == "/forecast/hourly":
			v = read("hourly.json")
		case r.URL.Path == "/gridpoints":
			v = read("grid.json")
		case r.URL.Path == "/MapClick.php":
			v = read("mapclick.json") // LVN11, 35°F at 7:00 AM PDT
		case r.URL.Path == "/stations" && r.URL.Query().Get("state") == "":
			v = read("stations.json") // the point's list: LVN11, TDRYD, TBLEW, STS54, KEAT
		case r.URL.Path == "/stations" && r.URL.Query().Get("state") == "WA" && !withStations:
			v = map[string]any{"features": []any{}}
		case r.URL.Path == "/stations" && r.URL.Query().Get("state") == "WA":
			// The state's stations: TUM42 is nearer the point than LVN11 but 3,000 ft higher.
			station := func(id string, lon, lat, elevM float64) map[string]any {
				return map[string]any{
					"geometry":   map[string]any{"coordinates": []float64{lon, lat}},
					"properties": map[string]any{"stationIdentifier": id, "name": id, "elevation": map[string]any{"value": elevM}},
				}
			}
			v = map[string]any{"features": []any{
				station("TUM42", -120.6615, 47.5970, 1274), station("LVN11", -120.6700, 47.5990, 362.7),
				station("KEAT", -120.2068, 47.3977, 374), // 39 km away
			}}
		case strings.HasSuffix(r.URL.Path, "/observations") && strings.Contains(r.URL.Path, "/LVN11/"):
			// LVN11 hourly from 1 to 7 AM PDT, 30-36°F (as °C, newest first like the API).
			feats := []any{}
			for h := 14; h >= 8; h-- {
				c := (30.0 + float64(h-8) - 32) * 5 / 9
				feats = append(feats, map[string]any{"properties": map[string]any{
					"timestamp": fmt.Sprintf("2026-10-25T%02d:00:00Z", h), "temperature": map[string]any{"value": c}}})
			}
			v = map[string]any{"features": feats}
		case strings.HasSuffix(r.URL.Path, "/observations") && strings.Contains(r.URL.Path, "/TUM42/"):
			v = map[string]any{"features": []any{map[string]any{"properties": map[string]any{
				"timestamp": "2026-10-25T14:00:00Z", "temperature": map[string]any{"value": -6.7},
				"windGust": map[string]any{"value": 48.3, "unitCode": "wmoUnit:km_h-1"}}}}}
		case strings.HasPrefix(r.URL.Path, "/stations/"):
			id := strings.Split(r.URL.Path, "/")[2]
			if id != "KEAT" {
				id = "LVN11" // the API's copy of every other station stopped on Oct 22
			}
			v = read("obs_" + id + ".json")
		case r.URL.Path == "/snotel/stations":
			v2 := []any{} // no SNOTEL station near Leavenworth
			json.NewEncoder(w).Encode(v2)
			return
		case r.URL.Path == "/alerts/active":
			v = map[string]any{"features": []any{map[string]any{"properties": map[string]any{
				"event": "Winter Weather Advisory", "severity": "Moderate", "status": "Actual", "messageType": "Alert"}}}}
		default:
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(v)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func buildFromFixtures(t *testing.T, dropPeriods int, now time.Time) *Glance {
	t.Helper()
	srv := fakeNWS(t, dropPeriods, false)
	oldNWS, oldForecast, oldSnotel := nwsBase, forecastBase, snotelBase
	nwsBase, forecastBase, snotelBase = srv.URL, srv.URL, srv.URL+"/snotel"
	t.Cleanup(func() { nwsBase, forecastBase, snotelBase = oldNWS, oldForecast, oldSnotel })

	g, err := BuildGlance(context.Background(), newNWSClient(), Options{Lat: 47.5962, Lon: -120.6615, Units: "us", Days: 7}, now)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestBuildGlanceMorning(t *testing.T) {
	now := time.Date(2026, 10, 25, 14, 45, 0, 0, time.UTC) // 7:45 AM PDT
	g := buildFromFixtures(t, 0, now)

	if g.Location != "Leavenworth, WA" || g.Unit != "F" || g.Updated != "7:45 AM" {
		t.Errorf("header = %q %q %q", g.Location, g.Unit, g.Updated)
	}
	// Measured 35°F at LVN11 (via forecast.weather.gov), not the hourly forecast's 28°F.
	if g.Now.Temp != 35 || g.Now.Source != "LVN11" || g.Now.ObservedAt != "7:00 AM" || g.Now.Humidity != 77 {
		t.Errorf("now = %+v", g.Now)
	}
	// The hour is Partly Sunny, but a trace of rain is forecast for this evening, so the icon shows it.
	if g.Now.Short != "Partly Sunny" || g.Now.Icon != "sprinkles" || g.Now.Wind != "Calm" {
		t.Errorf("now = %+v", g.Now)
	}
	if len(g.Days) != 7 {
		t.Fatalf("got %d days, want 7", len(g.Days))
	}
	first := g.Days[0]
	if first.Label != "Sun 25" || first.Hi != 55 || first.Lo != 34 {
		t.Errorf("first day = %+v", first)
	}
	if g.Scale.Min != 25 || g.Scale.Max != 56 {
		t.Errorf("scale = %+v", g.Scale)
	}
	for _, d := range g.Days {
		if d.TopPct < 0 || d.TopPct+d.HeightPct > 100.01 {
			t.Errorf("%s pill out of range: top %v height %v", d.Label, d.TopPct, d.HeightPct)
		}
	}
	// Leavenworth's grid has rain but no snow, so every bar is a rain bar.
	c := g.Chart
	if c == nil || c.BarKind != "auto" || len(c.Bars) != 28 || c.Days != 7 || c.TempPath == "" || c.FreezeY == nil {
		t.Fatalf("chart = %+v", c)
	}
	if g.Days[1].Rain != "0.16″" || g.Days[0].Rain != "" || g.Days[1].Snow != "" || c.Label != "Rain 0.36″" {
		t.Errorf("Monday rain/snow = %q/%q, label %q", g.Days[1].Rain, g.Days[1].Snow, c.Label)
	}
	if c.NowPct < 4 || c.NowPct > 5 { // 7:45 AM on day 1 of 7
		t.Errorf("now_pct = %v", c.NowPct)
	}
	if len(g.Alerts) != 1 || g.Alerts[0].Event != "Winter Weather Advisory" {
		t.Errorf("alerts = %+v", g.Alerts)
	}
}

func TestBuildGlanceEvening(t *testing.T) {
	// Starting at "Tonight": today's high comes from the hourly forecast.
	now := time.Date(2026, 10, 26, 1, 30, 0, 0, time.UTC) // 6:30 PM PDT
	g := buildFromFixtures(t, 1, now)

	first := g.Days[0]
	if first.Label != "Sun 25" || first.Lo != 34 || first.Hi < 34 {
		t.Errorf("first day = %+v", first)
	}
	if g.Updated != "6:30 PM" {
		t.Errorf("updated = %q", g.Updated)
	}
	// Every observation is hours old by now, so fall back to the hourly forecast.
	if g.Now.Source != "forecast" {
		t.Errorf("now = %+v", g.Now)
	}
}

func TestBuildGlanceStationFallback(t *testing.T) {
	// 9:05 AM: the MapClick reading (7:00) is too old, but KEAT reported at 7:40.
	now := time.Date(2026, 10, 25, 16, 5, 0, 0, time.UTC)
	g := buildFromFixtures(t, 0, now)
	if g.Now.Source != "KEAT" || g.Now.Temp != 37 || g.Now.ObservedAt != "7:40 AM" {
		t.Errorf("now = %+v", g.Now)
	}
}

func TestParseMapClickDate(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	got, err := parseMapClickDate("25 Oct 07:00 AM PDT", loc, time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC))
	if err != nil || !got.Equal(time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("got %v, %v", got, err)
	}
	// A Dec 31 reading seen on Jan 1 belongs to the previous year.
	got, _ = parseMapClickDate("31 Dec 11:00 PM PST", loc, time.Date(2027, 1, 1, 8, 30, 0, 0, time.UTC))
	if got.Year() != 2026 {
		t.Errorf("got %v", got)
	}
}

func TestParseRequest(t *testing.T) {
	q := url.Values{"lat_lon": {"47.5962, -120.6615"}, "units": {"si"}, "days": {"5"}}
	opts, multi, err := parseRequest(q)
	if err != nil || multi || len(opts) != 1 {
		t.Fatalf("parseRequest = %+v, %v, %v", opts, multi, err)
	}
	if o := opts[0]; o.Lat != 47.5962 || o.Lon != -120.6615 || o.Units != "si" || o.Days != 5 || o.Bars != "auto" {
		t.Errorf("opts = %+v", o)
	}
	if _, _, err := parseRequest(url.Values{"lat_lon": {"nope"}}); err == nil {
		t.Error("expected error for bad lat_lon")
	}

	q = url.Values{"locations": {"Leavenworth, WA | 47.5962,-120.6615\n Stevens Pass|47.7448,-121.0890;\n47.1,-121.2"}, "bars": {"snow"}}
	opts, multi, err = parseRequest(q)
	if err != nil || !multi || len(opts) != 3 {
		t.Fatalf("parseRequest = %+v, %v, %v", opts, multi, err)
	}
	if opts[0].Name != "Leavenworth, WA" || opts[1].Name != "Stevens Pass" || opts[1].Lon != -121.089 || opts[2].Name != "" || opts[2].Bars != "snow" {
		t.Errorf("opts = %+v", opts)
	}
	if _, _, err := parseRequest(url.Values{"locations": {"Home | here"}}); err == nil {
		t.Error("expected error for bad coordinates")
	}

	t.Setenv("ACCESS_TOKEN", "s3cret")
	if _, _, err := parseRequest(q); err == nil {
		t.Error("expected token error")
	}
	q.Set("token", "s3cret")
	if _, _, err := parseRequest(q); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
}

func TestHandler(t *testing.T) {
	srv := fakeNWS(t, 0, false)
	oldNWS, oldForecast, oldSnotel := nwsBase, forecastBase, snotelBase
	nwsBase, forecastBase, snotelBase = srv.URL, srv.URL, srv.URL+"/snotel"
	t.Cleanup(func() { nwsBase, forecastBase, snotelBase = oldNWS, oldForecast, oldSnotel })

	rec := httptest.NewRecorder()
	Handler(rec, httptest.NewRequest("GET", "/?lat_lon=47.5962,-120.6615&name=Home", nil))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, headers %v", rec.Code, rec.Header())
	}
	var g Glance
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil || g.Location != "Home" || len(g.Days) == 0 {
		t.Errorf("body = %s", rec.Body)
	}

	rec = httptest.NewRecorder()
	Handler(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != 400 {
		t.Errorf("missing lat_lon status = %d", rec.Code)
	}
}

func TestParseInterval(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"2026-10-25T12:00:00+00:00/PT6H":   6 * time.Hour,
		"2026-10-25T12:00:00+00:00/P1D":    24 * time.Hour,
		"2026-10-25T12:00:00+00:00/P1DT6H": 30 * time.Hour,
		"2026-10-25T12:00:00+00:00/PT30M":  30 * time.Minute,
	} {
		start, end, err := parseInterval(in)
		if err != nil || end.Sub(start) != want || start.Hour() != 12 {
			t.Errorf("parseInterval(%q) = %v, %v, %v", in, start, end, err)
		}
	}
	if _, _, err := parseInterval("2026-10-25T12:00:00+00:00"); err == nil {
		t.Error("expected error without a duration")
	}
}

func TestBuildChartRainAndSnow(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC) // 8 AM PDT
	var grid GridData
	// Temperatures 0°C, 5°C, -5°C in 12h steps from local midnight. Day 1, 6 pm-midnight local:
	// 12.7 mm (0.5") of rain. Day 2, noon-midnight local: 50.8 mm (2") of snow from 5.08 mm of water.
	if err := json.Unmarshal([]byte(`{"properties":{
		"temperature":{"values":[
			{"validTime":"2026-10-25T07:00:00+00:00/PT12H","value":0},
			{"validTime":"2026-10-25T19:00:00+00:00/PT12H","value":5},
			{"validTime":"2026-10-26T07:00:00+00:00/PT12H","value":-5}]},
		"snowfallAmount":{"values":[{"validTime":"2026-10-26T19:00:00+00:00/PT12H","value":50.8}]},
		"quantitativePrecipitation":{"values":[
			{"validTime":"2026-10-26T01:00:00+00:00/PT6H","value":12.7},
			{"validTime":"2026-10-26T19:00:00+00:00/PT12H","value":5.08}]}}}`), &grid); err != nil {
		t.Fatal(err)
	}
	days := make([]Day, 3)

	// auto: rain buckets as rain bars, snow buckets as snow bars, on fixed scales where half
	// height is 0.3″ of rain or 3″ of snow per 6 hours: 0.5″ of rain is 62.5%, 1″ of snow 12.5%.
	c := buildChart(&grid, days, loc, now, "us", "auto", nil, nil, 0)
	if c.BarKind != "auto" || len(c.Bars) != 12 {
		t.Fatalf("chart = %+v", c)
	}
	want := map[int]Bar{3: {62.5, "rain", "gray-55", false}, 6: {12.5, "snow", "gray-35", false}, 7: {12.5, "snow", "gray-35", false}}
	for i, b := range c.Bars {
		if b != want[i] {
			t.Errorf("bar %d = %+v, want %+v", i, b, want[i])
		}
	}
	if !reflect.DeepEqual(days[0].Amounts, []string{"0.50″ rain"}) || !reflect.DeepEqual(days[1].Amounts, []string{"2.0″ snow"}) ||
		len(days[2].Amounts) != 0 {
		t.Errorf("amounts = %q / %q / %q", days[0].Amounts, days[1].Amounts, days[2].Amounts)
	}
	if c.TempMin != 20 || c.TempMax != 45 || c.FreezeY == nil || c.Label != "Snow 2.0″ · Rain 0.50″" {
		t.Errorf("scale %d-%d freeze %v label %q", c.TempMin, c.TempMax, c.FreezeY, c.Label)
	}

	// snow: snowfall only.
	days = make([]Day, 3)
	c = buildChart(&grid, days, loc, now, "us", "snow", nil, nil, 0)
	if c.Bars[3] != (Bar{}) || c.Bars[6].Kind != "snow" || len(days[0].Amounts) != 0 || c.Label != "Snow 2.0″" {
		t.Errorf("snow-only chart = %+v, days %+v", c, days)
	}

	// precip: all liquid, snow included as water (0.1″ per bucket), on the rain scale.
	days = make([]Day, 3)
	c = buildChart(&grid, days, loc, now, "us", "precip", nil, nil, 0)
	if c.Bars[3].Height != 62.5 || c.Bars[6] != (Bar{12.5, "rain", "gray-55", false}) || c.Label != "Precip 0.70″" ||
		!reflect.DeepEqual(days[1].Amounts, []string{"0.20″ precip"}) {
		t.Errorf("precip chart = %+v, days %+v", c, days)
	}

	days = make([]Day, 3)
	c = buildChart(&grid, days, loc, now, "si", "auto", nil, nil, 0)
	if c.TempMin != -10 || c.Freeze != "0°" || c.Label != "Snow 5.1cm · Rain 12.7mm" ||
		!reflect.DeepEqual(days[0].Amounts, []string{"12.7mm rain"}) {
		t.Errorf("metric chart label %q, days %+v", c.Label, days)
	}
}

func TestMapClickRejectsDistantElevation(t *testing.T) {
	// Stevens Pass (4879 ft) gets Skykomish's SKKW1 (2028 ft) as its MapClick station.
	b, err := os.ReadFile("testdata/mapclick_stevens.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(b) }))
	defer srv.Close()
	old := forecastBase
	forecastBase = srv.URL
	defer func() { forecastBase = old }()

	loc, _ := time.LoadLocation("America/Los_Angeles")
	_, err = newNWSClient().mapClickObservation(context.Background(), 47.7448, -121.089, loc, time.Now())
	if err == nil || !strings.Contains(err.Error(), "SKKW1") {
		t.Errorf("expected elevation rejection, got %v", err)
	}
}

func TestServeMultipleLocations(t *testing.T) {
	srv := fakeNWS(t, 0, false)
	oldNWS, oldForecast, oldSnotel := nwsBase, forecastBase, snotelBase
	nwsBase, forecastBase, snotelBase = srv.URL, srv.URL, srv.URL+"/snotel"
	t.Cleanup(func() { nwsBase, forecastBase, snotelBase = oldNWS, oldForecast, oldSnotel })

	status, body := Serve(context.Background(), url.Values{"locations": {"Leavenworth, WA | 47.5962,-120.6615\nUp the hill | 47.7448,-121.0890"}})
	var resp struct {
		Count     int      `json:"count"`
		Locations []Glance `json:"locations"`
	}
	if err := json.Unmarshal(body, &resp); err != nil || status != 200 || resp.Count != 2 {
		t.Fatalf("status %d, body %s", status, body)
	}
	if resp.Locations[0].Location != "Leavenworth, WA" || resp.Locations[1].Location != "Up the hill" || resp.Locations[1].Chart == nil {
		t.Errorf("locations = %+v", resp.Locations)
	}
}

func TestShareScale(t *testing.T) {
	valley := &Chart{samples: []sample{{0, 30}, {500, 56}}, freeze: 32, NowX: 250}
	pass := &Chart{samples: []sample{{0, 22}, {500, 41}}, freeze: 32}
	ShareScale([]*Chart{valley, nil, pass})

	for _, c := range []*Chart{valley, pass} {
		if c.TempMin != 20 || c.TempMax != 60 || c.FreezeY == nil || *c.FreezeY != 70 {
			t.Errorf("chart scale = %d..%d, freeze %v", c.TempMin, c.TempMax, c.FreezeY)
		}
	}
	// Halfway between 30° and 56° is 43°, 42.5% down a 20..60 scale.
	if valley.NowY == nil || *valley.NowY != 42.5 {
		t.Errorf("valley now_y = %v", valley.NowY)
	}
	// 41° on a 20..60 scale sits 47.5% down from the top.
	if pass.TempPath != "M0.0,95.0L500.0,47.5" {
		t.Errorf("path = %q", pass.TempPath)
	}
}

func TestBarScale(t *testing.T) {
	// Snow per 6 hours: linear, 6″ makes the tallest bar (75% of the chart), more stops there.
	for _, tc := range []struct{ amount, want float64 }{{0, 0}, {1, 12.5}, {1.5, 18.8}, {3, 37.5}, {4.5, 56.3}, {6, 75}, {24, 75}} {
		if got := barHeight(tc.amount, snowHalfIn); got != tc.want {
			t.Errorf("snow %v″ = %v%%, want %v%%", tc.amount, got, tc.want)
		}
		if got := barHeight(tc.amount/10, rainHalfIn); got != tc.want {
			t.Errorf("rain %v″ = %v%%, want %v%%", tc.amount/10, got, tc.want)
		}
	}
}

func TestTotalsMatchDayLabels(t *testing.T) {
	// Three days of 0.084″ of rain each show as 0.08″; the total must read 0.24″, not 0.25″.
	var grid GridData
	if err := json.Unmarshal([]byte(`{"properties":{"quantitativePrecipitation":{"values":[
		{"validTime":"2026-10-25T07:00:00+00:00/PT6H","value":2.1336},
		{"validTime":"2026-10-26T07:00:00+00:00/PT6H","value":2.1336},
		{"validTime":"2026-10-27T07:00:00+00:00/PT6H","value":2.1336}]}}}`), &grid); err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	days := make([]Day, 3)
	c := buildChart(&grid, days, loc, time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC), "us", "auto", nil, nil, 0)
	if days[0].Rain != "0.08″" || c.Label != "Rain 0.24″" {
		t.Errorf("day %q, label %q", days[0].Rain, c.Label)
	}
}

func TestLightAmounts(t *testing.T) {
	for _, tc := range []struct {
		v     float64
		kind  string
		units string
		want  string
	}{
		{0, "rain", "us", ""},
		{0.002, "rain", "us", "Sprinkles"}, // a trace too small to show as a number
		{0.02, "rain", "us", "Sprinkles"},
		{0.09, "rain", "us", "Sprinkles"},
		{0.1, "rain", "us", "0.10″ rain"},
		{0.095, "rain", "us", "0.10″ rain"}, // judged as shown, not as measured
		{0.94, "snow", "us", "Flurries"},
		{0.96, "snow", "us", "1.0″ snow"},
		{0.8, "snow", "us", "Flurries"},
		{1, "snow", "us", "1.0″ snow"},
		{2, "rain", "si", "Sprinkles"},
		{2.5, "rain", "si", "2.5mm rain"},
		{2, "snow", "si", "Flurries"},
		{2.5, "snow", "si", "2.5cm snow"},
	} {
		light, limit := "Sprinkles", sprinklesIn
		switch {
		case tc.kind == "snow" && tc.units == "si":
			light, limit = "Flurries", flurriesCm
		case tc.kind == "snow":
			light, limit = "Flurries", flurriesIn
		case tc.units == "si":
			limit = sprinklesMm
		}
		if got := describeAmount(tc.v, roundAmount(tc.v, tc.kind, tc.units), formatAmount(tc.v, tc.kind, tc.units), tc.kind, light, limit); got != tc.want {
			t.Errorf("%v %s (%s) = %q, want %q", tc.v, tc.kind, tc.units, got, tc.want)
		}
	}
}

func TestCurrentIcon(t *testing.T) {
	chart := func(snow, rain float64) *Chart {
		return &Chart{restSnow: snow, restRain: rain, flurries: flurriesIn, sprinkles: sprinklesIn}
	}
	for _, tc := range []struct {
		name       string
		icon       string
		short      string
		snow, rain float64
		temp       int
		want       string
	}{
		{"Stevens Pass, 40% rain and snow, nothing measurable, 39°F", "snow", "Chance Rain And Snow", 0, 0, 39, "sprinkles"},
		{"same at 31°F", "snow", "Chance Rain And Snow", 0, 0, 31, "flurries"},
		{"snow wording, nothing measurable", "snow", "Chance Snow", 0, 0, 30, "flurries"},
		{"rain wording, nothing measurable", "rain", "Chance Rain Showers", 0, 0, 50, "sprinkles"},
		{"sunny now, trace of rain this evening", "partly", "Mostly Sunny", 0, 0.002, 37, "sprinkles"},
		{"cloudy now, real rain later", "cloudy", "Mostly Cloudy", 0, 0.2, 45, "rain"},
		{"snowing, 2 inches to come", "snow", "Snow", 2, 0, 28, "snow"},
		{"light snow", "flurries", "Light Snow Likely", 0.1, 0, 30, "flurries"},
		{"rain and snow both measurable", "snow", "Rain And Snow", 1, 0.1, 34, "sleet"},
		{"rain wording but the grid says snow", "rain", "Rain Showers", 1, 0, 33, "snow"},
		{"clear, nothing today", "clear", "Sunny", 0, 0, 60, "clear"},
		{"thunderstorms", "storm", "Showers And Thunderstorms", 0, 0.5, 70, "storm"},
	} {
		if got := currentIcon(tc.icon, tc.short, chart(tc.snow, tc.rain), tc.temp, "us"); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestPickStation(t *testing.T) {
	now := time.Date(2026, 11, 4, 5, 0, 0, 0, time.UTC)
	fresh, stale := []time.Time{now.Add(-30 * time.Minute)}, []time.Time{now.Add(-3 * time.Hour)}
	series := []stationSeries{
		{ID: "STALE", ElevationFt: 3950, Times: stale, TempF: []float64{30}},
		{ID: "LOW", ElevationFt: 2028, Times: fresh, TempF: []float64{45}},
		{ID: "STS40", ElevationFt: 3950, Times: fresh, TempF: []float64{35}},
	}
	// Stevens Pass's grid point is ~1487 m (4880 ft): skip the stale and the far-lower stations.
	if s := pickStation(series, 1487, now); s == nil || s.ID != "STS40" {
		t.Errorf("picked %+v", s)
	}
	if s := pickStation(series[:2], 1487, now); s != nil {
		t.Errorf("expected none, picked %s", s.ID)
	}
	// Unknown point elevation: the nearest fresh station.
	if s := pickStation(series, math.NaN(), now); s == nil || s.ID != "LOW" {
		t.Errorf("picked %+v", s)
	}
}

func TestMergeObserved(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 11, 3, 18, 0, 0, 0, time.UTC) // 10 AM PST
	var grid GridData
	// The forecast starts at 10 AM local (18:00Z), as NWS's do when issued mid-morning.
	if err := json.Unmarshal([]byte(`{"properties":{"temperature":{"values":[
		{"validTime":"2026-11-03T18:00:00+00:00/PT6H","value":10}]}}}`), &grid); err != nil {
		t.Fatal(err)
	}
	obs := &stationSeries{ID: "LVN11"}
	for h := 0; h < 10; h++ { // midnight-9 AM local readings, every 30 minutes
		for _, m := range []int{0, 30} {
			obs.Times = append(obs.Times, time.Date(2026, 11, 3, h, m, 0, 0, loc))
			obs.TempF = append(obs.TempF, 30+float64(h))
		}
	}
	c := buildChart(&grid, make([]Day, 1), loc, now, "us", "auto", obs, nil, 0)
	if len(c.samples) < 12 {
		t.Fatalf("samples = %v", c.samples)
	}
	first := c.samples[0]
	if math.Abs(first.x-1000*0.5/24) > 0.01 || first.t != 30 {
		t.Errorf("first sample = %+v, want 12:30 AM at 30°F", first)
	}
	// Past hours come from the station, then the latest reading, then the forecast (50°F).
	if got := c.samples[9]; got.t != 39 || c.samples[10].t != 39 || c.samples[11].t != 50 {
		t.Errorf("samples around now = %+v %+v %+v", c.samples[9], c.samples[10], c.samples[11])
	}
	if c.NowY == nil {
		t.Error("now_y not set")
	}
}

func TestBuildGlanceWithStations(t *testing.T) {
	srv := fakeNWS(t, 0, true)
	oldNWS, oldForecast, oldSnotel := nwsBase, forecastBase, snotelBase
	nwsBase, forecastBase, snotelBase = srv.URL, srv.URL, srv.URL+"/snotel"
	t.Cleanup(func() { nwsBase, forecastBase, snotelBase = oldNWS, oldForecast, oldSnotel })

	now := time.Date(2026, 10, 25, 14, 45, 0, 0, time.UTC) // 7:45 AM PDT
	g, err := BuildGlance(context.Background(), newNWSClient(), Options{Lat: 47.5962, Lon: -120.6615, Units: "us", Days: 7}, now)
	if err != nil {
		t.Fatal(err)
	}
	// TUM42 is closer but 3,000 ft higher, so LVN11's 7 AM reading (36°F) wins.
	if g.Now.Source != "LVN11" || g.Now.Temp != 36 || g.Now.ObservedAt != "7:00 AM" {
		t.Errorf("now = %+v", g.Now)
	}
	// Today's 30-36°F readings pull the forecast's low (34°) down to 30°; the high (55°) stands.
	if g.Days[0].Lo != 30 || g.Days[0].Hi != 55 {
		t.Errorf("today lo/hi = %d/%d", g.Days[0].Lo, g.Days[0].Hi)
	}
	// The line now starts at 1:30 AM (the first full hour of readings) instead of the forecast's start.
	if x := g.Chart.samples[0].x; math.Abs(x-1000*1.5/(24*7)) > 0.01 {
		t.Errorf("first sample x = %v", x)
	}
}

// A week of real Stevens Pass readings (Oct 27 - Nov 3, 2026): Grace Lakes measured a ~7″ storm
// on Oct 28, Brooks' depth sensor read 270″ for three days, Schmidt Haus' gauge read zero all
// week, and sunny afternoons nudged depths up an inch at 35-45°F.
func TestObservedRealStevensWeek(t *testing.T) {
	b, err := os.ReadFile("testdata/synoptic_stevens_week.json")
	if err != nil {
		t.Fatal(err)
	}
	// Recorded from Synoptic's timeseries API, which the service no longer uses.
	r := &struct {
		Station []struct {
			STID         string      `json:"STID"`
			Elevation    looseNumber `json:"ELEVATION"`
			Observations struct {
				DateTime  []time.Time `json:"date_time"`
				AirTemp   []*float64  `json:"air_temp_set_1"`
				SnowDepth []*float64  `json:"snow_depth_set_1"`
				Precip    []*float64  `json:"precip_accum_one_hour_set_1"`
			} `json:"OBSERVATIONS"`
		} `json:"STATION"`
	}{}
	if err := json.Unmarshal(b, r); err != nil {
		t.Fatal(err)
	}
	series := []stationSeries{}
	for _, s := range r.Station {
		ss := stationSeries{ID: s.STID}
		ss.ElevationFt, _ = s.Elevation.Float64()
		o := s.Observations
		val := func(v []*float64, i int) *float64 {
			if i < len(v) {
				return v[i]
			}
			return nil
		}
		for i, tm := range o.DateTime {
			if v := val(o.AirTemp, i); v != nil {
				ss.Times, ss.TempF = append(ss.Times, tm), append(ss.TempF, *v)
			}
			if v := val(o.SnowDepth, i); v != nil {
				ss.Depth = append(ss.Depth, reading{tm, *v})
			}
			if v := val(o.Precip, i); v != nil {
				ss.Precip = append(ss.Precip, reading{tm, *v})
			}
		}
		series = append(series, ss)
	}
	o := newObserved(series, 1487)
	if o == nil || len(o.gauges) != 2 {
		t.Fatalf("observed = %+v", o)
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	day := func(d int) (time.Time, time.Time) {
		a := time.Date(2026, 10, d, 0, 0, 0, 0, loc)
		return a, a.AddDate(0, 0, 1)
	}
	if a, b := day(28); math.Abs(o.snow(a, b, barSnowMinRise)-6.9) > 0.3 || math.Abs(o.rain(a, b)-0.36) > 0.02 {
		t.Errorf("Oct 28: snow %.1f, rain %.2f; want ~6.9″ and 0.36″", o.snow(a, b, barSnowMinRise), o.rain(a, b))
	}
	// Warm, dry afternoons: no snow.
	for _, d := range []int{33, 34} { // Nov 2, Nov 3
		if a, b := day(d); o.snow(a, b, barSnowMinRise) != 0 {
			t.Errorf("Oct %d (Nov): snow %.1f, want 0", d, o.snow(a, b, barSnowMinRise))
		}
	}
	noon29 := time.Date(2026, 10, 29, 12, 0, 0, 0, loc)
	if f := o.snow(noon29.Add(-48*time.Hour), noon29, freshSnowMinRise); math.Abs(f-7.1) > 0.3 {
		t.Errorf("fresh 48h at Oct 29 noon = %.1f, want ~7.1", f)
	}

	// A chart starting the day before Oct 29 noon: Oct 28 is measured, outlined bars and all.
	// It all fell before Oct 28 noon.
	if f := freshSnow(o, noon29, "us"); f != "7″ 48h" {
		t.Errorf("freshSnow = %q", f)
	}
	var st *stationSeries
	for i := range series {
		if series[i].ID == "STS40" {
			st = &series[i]
		}
	}
	yday, ok := observedDay(st, time.Date(2026, 10, 28, 0, 0, 0, 0, loc), "us")
	if !ok || yday.Dow != "Wed" || yday.Hi <= yday.Lo {
		t.Fatalf("observedDay = %+v, %v", yday, ok)
	}
	days := []Day{yday, {}, {}}
	c := buildChart(&GridData{}, days, loc, noon29, "us", "auto", st, o, 1)
	if c.Days != 3 || math.Abs(c.NowX-500) > 0.1 {
		t.Errorf("chart days %d, now %.1f", c.Days, c.NowX)
	}
	for k, bar := range c.Bars {
		if past := k < 6; bar.Observed != (past && bar.Kind != "") {
			t.Errorf("bar %d = %+v", k, bar)
		}
	}
	if len(days[0].Amounts) == 0 || !strings.Contains(days[0].Amounts[0], "snow") {
		t.Errorf("Oct 28 amounts = %v", days[0].Amounts)
	}
}

func TestDepthRiseIgnoresFaults(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 1, h, 0, 0, 0, time.UTC) }
	raw := []reading{}
	for h := 0; h < 12; h++ {
		v := 10.0
		switch {
		case h == 3:
			v = 30 // single-hour spike
		case h >= 8:
			v = 270 // sensor jumps to a bogus level and stays there
		}
		raw = append(raw, reading{at(h), v})
	}
	d := depthSeries{depth: smoothDepth(raw)}
	if r := depthRise(d, at(0), at(11)); r != 0 {
		t.Errorf("rise = %v, want 0 (spike and level jump are faults)", r)
	}
	// A real 4″ storm over four hours at 28°F counts; the same rise at 40°F doesn't.
	storm := []reading{}
	temps := map[int64]float64{}
	for h := 0; h < 10; h++ {
		storm = append(storm, reading{at(h), 10 + math.Min(math.Max(float64(h-2), 0), 4)})
		temps[at(h).Unix()] = 28
	}
	if r := depthRise(depthSeries{smoothDepth(storm), temps}, at(0), at(9)); r != 4 {
		t.Errorf("storm rise = %v, want 4", r)
	}
	for k := range temps {
		temps[k] = 40
	}
	if r := depthRise(depthSeries{smoothDepth(storm), temps}, at(0), at(9)); r != 0 {
		t.Errorf("warm rise = %v, want 0", r)
	}
}

func TestSnowNeedsTwoStations(t *testing.T) {
	at := func(h int) time.Time { return time.Date(2026, 11, 1, h, 0, 0, 0, time.UTC) }
	series := func(id string, f func(h int) float64) stationSeries {
		s := stationSeries{ID: id, ElevationFt: 4000}
		for h := 0; h < 24; h++ {
			s.Times, s.TempF = append(s.Times, at(h)), append(s.TempF, 30)
			s.Depth = append(s.Depth, reading{at(h), f(h)})
		}
		return s
	}
	flat := func(int) float64 { return 5 }
	storm := func(h int) float64 { return 5 + 0.3*float64(max(0, min(h, 18)-6)) } // 3.6″ by 6 PM
	toggling := func(h int) float64 { return float64(1 + (h/3)%2) }               // whole inches, 1 ↔ 2
	noisy := func(h int) float64 { return 6 + 7*float64(h%2) }                    // 6 ↔ 13 every hour

	from, to := at(0), at(23)
	for _, tc := range []struct {
		name string
		s    []stationSeries
		want float64
	}{
		{"one station alone", []stationSeries{series("A", storm)}, 3.6},
		{"corroborated", []stationSeries{series("A", storm), series("B", storm), series("C", flat)}, 3.6},
		{"lone sensor", []stationSeries{series("A", toggling), series("B", flat)}, 0},
		{"noisy station ignored", []stationSeries{series("A", noisy), series("B", flat)}, 0},
	} {
		if got := newObserved(tc.s, math.NaN()).snow(from, to, barSnowMinRise); math.Abs(got-tc.want) > 0.05 {
			t.Errorf("%s: snow = %.2f, want %.1f", tc.name, got, tc.want)
		}
	}
}

func TestWind(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, loc)
	at := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, loc) }
	grid := GridData{}
	add := func(t time.Time, kmh float64) {
		grid.Properties.WindGust.Values = append(grid.Properties.WindGust.Values, struct {
			ValidTime string   `json:"validTime"`
			Value     *float64 `json:"value"`
		}{t.Format(time.RFC3339) + "/PT3H", &kmh})
	}
	add(at(2, 0), 80)  // 50 mph, but already past: the stations say otherwise
	add(at(2, 12), 30) // 19 mph: not windy
	add(at(3, 6), 64)  // 40 mph
	seen := &observed{gusts: [][]reading{
		{{at(2, 3), 20}, {at(2, 7), 31}},
		{{at(2, 4), 26}, {at(2, 8), 45}},
		{{at(2, 5), 10}},
	}}
	days := make([]Day, 2)
	c := buildChart(&grid, days, loc, now, "us", "auto", nil, seen, 0)
	// Block 0 (midnight-6): station peaks 20, 26, 10 -> median 20, calm. Block 1: 31, 45 -> 38.
	// Two windy spells (blocks 1 and 5), each a curve easing in from zero at the middle of the
	// block before, through the gust, and back out to zero at the middle of the block after.
	if c.WindLine != "M62.5,0C104.2,0 145.8,63.3 187.5,63.3C229.2,63.3 270.8,0 312.5,0"+
		"M562.5,0C604.2,0 645.8,66.3 687.5,66.3C729.2,66.3 770.8,0 812.5,0" ||
		days[0].Gust != 38 || days[1].Gust != 40 || len(c.Gusts) != 2 || c.Gusts[1].Labels[0] != "40" {
		t.Errorf("wind = %s, gusts %d / %d, labels %+v", c.WindLine, days[0].Gust, days[1].Gust, c.Gusts)
	}
	days = make([]Day, 2)
	buildChart(&grid, days, loc, now, "si", "auto", nil, seen, 0)
	if days[1].Gust != 64 {
		t.Errorf("si gust = %d", days[1].Gust)
	}
}

func TestStorms(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, loc)
	grid := GridData{}
	add := func(series *GridSeries, h int, v float64) {
		series.Values = append(series.Values, struct {
			ValidTime string   `json:"validTime"`
			Value     *float64 `json:"value"`
		}{time.Date(2026, 11, 2, h, 0, 0, 0, loc).Format(time.RFC3339) + "/PT6H", &v})
	}
	q := &grid.Properties.QuantitativePrecipitation
	add(q, 0, 0.2)   // 0.008″: too light to count
	add(q, 6, 2.54)  // 0.10″ \ one storm, blocks 1-2 (6 AM-6 PM), centered at 25%: 0.30″
	add(q, 12, 5.08) // 0.20″ /
	add(q, 18, 0.25) // 0.01″: a break
	add(q, 24, 1.0)  // 0.04″: a storm too light to label
	add(q, 36, 7.62) // 0.30″ and snow: 3″
	add(&grid.Properties.SnowfallAmount, 36, 76.2)
	days := make([]Day, 2)
	c := buildChart(&grid, days, loc, now, "us", "auto", nil, nil, 0)
	want := []Storm{{X: 25, Labels: []string{"0.30″"}}, {X: 81.3, Labels: []string{"3.0″"}}}
	if !reflect.DeepEqual(c.Storms, want) {
		t.Errorf("storms = %+v", c.Storms)
	}
}

func TestBuildDaysOvernight(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	at := func(d, h int) time.Time { return time.Date(2026, 11, d, h, 0, 0, 0, loc) }
	p := func(name string, start time.Time, day bool, temp int) Period {
		return Period{Name: name, StartTime: start, EndTime: start.Add(6 * time.Hour), IsDaytime: day, Temperature: temp}
	}
	// Just after midnight: "Overnight" until 6 AM, then Monday. Monday must appear once.
	periods := []Period{
		p("Overnight", at(9, 1), false, 31),
		p("Monday", at(9, 6), true, 48),
		p("Monday Night", at(9, 18), false, 34),
		p("Tuesday", at(10, 6), true, 49),
		p("Tuesday Night", at(10, 18), false, 37),
	}
	days := buildDays(periods, nil, loc, 7)
	if len(days) != 2 || days[0].Label != "Mon 9" || days[1].Label != "Tue 10" ||
		days[0].Hi != 48 || days[0].Lo != 31 || days[1].Lo != 37 {
		t.Errorf("days = %+v", days)
	}
	// Before midnight, "Tonight" is still today's own entry.
	periods[0] = p("Tonight", at(8, 21), false, 31)
	days = buildDays(periods, nil, loc, 7)
	if len(days) != 3 || days[0].Label != "Sun 8" || days[1].Label != "Mon 9" {
		t.Errorf("tonight days = %+v", days)
	}
}

// A week of real hourly SNOTEL data from Stevens Pass (Nov 4-10, 2026). Midnight to midnight it
// measured 0.6″ on Nov 6 and 1.0″ on Nov 8 (the running total went 4.3″ to 5.3″).
func TestParseSnotel(t *testing.T) {
	b, err := os.ReadFile("testdata/snotel_stevens_week.json")
	if err != nil {
		t.Fatal(err)
	}
	d := snotelData{}
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	tz := time.FixedZone("PST", -8*3600)
	s := stationSeries{}
	parseSnotel(d, tz, &s)
	if len(s.Precip) < 100 || len(s.Times) < 100 || len(s.Depth) == 0 {
		t.Fatalf("parsed %d precip, %d temps, %d depths", len(s.Precip), len(s.Times), len(s.Depth))
	}
	day := func(dd int) float64 {
		from := time.Date(2026, 11, dd, 0, 0, 0, 0, tz)
		total := 0.0
		for _, r := range s.Precip {
			if r.T.After(from) && !r.T.After(from.Add(24*time.Hour)) {
				total += r.V
			}
		}
		return math.Round(total*10) / 10
	}
	if day(6) != 0.6 || day(8) != 1.0 {
		t.Errorf("Nov 6 = %.1f″, Nov 8 = %.1f″; want 0.6 and 1.0", day(6), day(8))
	}
	// A missing hour doesn't lose its rain: drop Nov 8's 1 AM reading (4.3″ -> 4.5″ at 1 AM).
	for _, st := range d {
		for i := range st.Data {
			vals := st.Data[i].Values
			for k := range vals {
				if vals[k].Date == "2026-11-08 01:00" {
					st.Data[i].Values = append(vals[:k:k], vals[k+1:]...)
					break
				}
			}
		}
	}
	s = stationSeries{}
	parseSnotel(d, tz, &s)
	if day(8) != 1.0 {
		t.Errorf("Nov 8 with a missing hour = %.1f″", day(8))
	}
	// As a gauge it feeds the measured rain: Nov 6's afternoon block.
	o := newObserved([]stationSeries{s}, math.NaN())
	if o == nil || len(o.gauges) != 1 {
		t.Fatalf("observed = %+v", o)
	}
	noon := time.Date(2026, 11, 6, 12, 0, 0, 0, tz)
	if r := o.rain(noon, noon.Add(6*time.Hour)); r < 0.3 {
		t.Errorf("Nov 6 noon-6 PM rain = %.2f", r)
	}
}
