package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
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
		case r.URL.Path == "/forecast":
			v = read("forecast.json")
			props := v["properties"].(map[string]any)
			props["periods"] = props["periods"].([]any)[dropPeriods:]
		case r.URL.Path == "/forecast/hourly":
			v = read("hourly.json")
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

func TestParseOptions(t *testing.T) {
	q := map[string][]string{"lat_lon": {"47.5962, -120.6615"}, "units": {"si"}, "days": {"5"}}
	opts, err := parseOptions(q)
	if err != nil || opts.Lat != 47.5962 || opts.Lon != -120.6615 || opts.Units != "si" || opts.Days != 5 {
		t.Errorf("parseOptions = %+v, %v", opts, err)
	}
	if _, err := parseOptions(map[string][]string{"lat_lon": {"nope"}}); err == nil {
		t.Error("expected error for bad lat_lon")
	}

	t.Setenv("ACCESS_TOKEN", "s3cret")
	if _, err := parseOptions(q); err == nil {
		t.Error("expected token error")
	}
	q["token"] = []string{"s3cret"}
	if _, err := parseOptions(q); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}
}

func TestLambdaHandler(t *testing.T) {
	srv := fakeNWS(t, 0)
	oldNWS, oldForecast := nwsBase, forecastBase
	nwsBase, forecastBase = srv.URL, srv.URL
	t.Cleanup(func() { nwsBase, forecastBase = oldNWS, oldForecast })

	resp, err := lambdaHandler(context.Background(), events.LambdaFunctionURLRequest{
		QueryStringParameters: map[string]string{"lat_lon": "47.5962,-120.6615", "name": "Home"},
	})
	if err != nil || resp.StatusCode != 200 || resp.Headers["Content-Type"] != "application/json" {
		t.Fatalf("resp = %+v, %v", resp, err)
	}
	var g Glance
	if err := json.Unmarshal([]byte(resp.Body), &g); err != nil || g.Location != "Home" || len(g.Days) == 0 {
		t.Errorf("body = %s", resp.Body)
	}

	resp, _ = lambdaHandler(context.Background(), events.LambdaFunctionURLRequest{})
	if resp.StatusCode != 400 {
		t.Errorf("missing lat_lon status = %d", resp.StatusCode)
	}
}
