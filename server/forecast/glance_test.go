package forecast

import (
	"context"
	"encoding/json"
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
	}
	for _, tc := range tests {
		icon, severity := classify(tc.short, tc.pop)
		if icon != tc.icon || severity != tc.severity {
			t.Errorf("classify(%q, %d) = %s, %d; want %s, %d", tc.short, tc.pop, icon, severity, tc.icon, tc.severity)
		}
	}
}

// fakeNWS serves the recorded Leavenworth responses in testdata/, optionally dropping
// leading forecast periods to simulate fetching later in the day.
func fakeNWS(t *testing.T, dropPeriods int) *httptest.Server {
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
		case r.URL.Path == "/stations":
			v = read("stations.json") // LVN11, TDRYD, TBLEW, STS54, KEAT
		case strings.HasPrefix(r.URL.Path, "/stations/"):
			id := strings.Split(r.URL.Path, "/")[2]
			if id != "KEAT" {
				id = "LVN11" // the API's copy of every other station stopped on Oct 22
			}
			v = read("obs_" + id + ".json")
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
	srv := fakeNWS(t, dropPeriods)
	oldNWS, oldForecast := nwsBase, forecastBase
	nwsBase, forecastBase = srv.URL, srv.URL
	t.Cleanup(func() { nwsBase, forecastBase = oldNWS, oldForecast })

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
	if g.Now.Icon != "partly" || g.Now.Wind != "Calm" {
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
	srv := fakeNWS(t, 0)
	oldNWS, oldForecast := nwsBase, forecastBase
	nwsBase, forecastBase = srv.URL, srv.URL
	t.Cleanup(func() { nwsBase, forecastBase = oldNWS, oldForecast })

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
	// height is 0.3″ of rain or 3″ of snow per 6 hours: 0.5″ of rain is 83.3%, 1″ of snow 16.7%.
	c := buildChart(&grid, days, loc, now, "us", "auto")
	if c.BarKind != "auto" || len(c.Bars) != 12 {
		t.Fatalf("chart = %+v", c)
	}
	want := map[int]Bar{3: {83.3, "rain", "gray-55"}, 6: {16.7, "snow", "gray-35"}, 7: {16.7, "snow", "gray-35"}}
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
	c = buildChart(&grid, days, loc, now, "us", "snow")
	if c.Bars[3] != (Bar{}) || c.Bars[6].Kind != "snow" || len(days[0].Amounts) != 0 || c.Label != "Snow 2.0″" {
		t.Errorf("snow-only chart = %+v, days %+v", c, days)
	}

	// precip: all liquid, snow included as water (0.1″ per bucket), on the rain scale.
	days = make([]Day, 3)
	c = buildChart(&grid, days, loc, now, "us", "precip")
	if c.Bars[3].Height != 83.3 || c.Bars[6] != (Bar{16.7, "rain", "gray-55"}) || c.Label != "Precip 0.70″" ||
		!reflect.DeepEqual(days[1].Amounts, []string{"0.20″ precip"}) {
		t.Errorf("precip chart = %+v, days %+v", c, days)
	}

	days = make([]Day, 3)
	c = buildChart(&grid, days, loc, now, "si", "auto")
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
	srv := fakeNWS(t, 0)
	oldNWS, oldForecast := nwsBase, forecastBase
	nwsBase, forecastBase = srv.URL, srv.URL
	t.Cleanup(func() { nwsBase, forecastBase = oldNWS, oldForecast })

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
	// Snow per 6 hours: linear with 3″ at half height, 6″ at full, more clipped to full.
	for _, tc := range []struct{ amount, want float64 }{{0, 0}, {1, 16.7}, {1.5, 25}, {3, 50}, {4.5, 75}, {6, 100}, {24, 100}} {
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
	c := buildChart(&grid, days, loc, time.Date(2026, 10, 25, 15, 0, 0, 0, time.UTC), "us", "auto")
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
		{0.03, "rain", "us", "0.03″ rain"},
		{0.1, "snow", "us", "Flurries"},
		{0.3, "snow", "us", "0.3″ snow"},
		{0.5, "rain", "si", "Sprinkles"},
		{2, "rain", "si", "2.0mm rain"},
		{0.5, "snow", "si", "Flurries"},
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
		if got := describeAmount(tc.v, formatAmount(tc.v, tc.kind, tc.units), tc.kind, light, limit); got != tc.want {
			t.Errorf("%v %s (%s) = %q, want %q", tc.v, tc.kind, tc.units, got, tc.want)
		}
	}
}
