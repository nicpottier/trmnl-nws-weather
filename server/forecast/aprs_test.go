package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCWOPCall(t *testing.T) {
	for id, want := range map[string]string{"E5752": "EW5752", "F4618": "FW4618", "LVN11": "", "KEAT": "", "E57521": ""} {
		if got := cwopCall(id); got != want {
			t.Errorf("cwopCall(%q) = %q, want %q", id, got, want)
		}
	}
}

func TestRainFromSamples(t *testing.T) {
	// A station whose "since midnight" total resets at UTC midnight: 4 PM in Leavenworth.
	loc, _ := time.LoadLocation("America/Los_Angeles")
	at := func(d, h, m int) int64 { return time.Date(2026, 11, d, h, m, 0, 0, loc).Unix() }
	samples := []aprsSample{
		{T: at(10, 15, 0), Mn: 2.0, R1: 0.5, R24: 4.8},
		{T: at(10, 15, 30), Mn: 3.0, R1: 1.0}, // +1.0 mm
		{T: at(10, 16, 20), Mn: 0.4, R1: 1.2}, // its reset: 0.4 since, plus 0.8 of the last hour before
		{T: at(10, 17, 0), Mn: 0.4},           // dry
		{T: at(10, 22, 0), Mn: 2.4},           // a 5-hour gap: 2.0 mm, spread over it
		{T: at(11, 6, 0), Mn: 0.2},            // a reset after an 8-hour gap: unknown
	}
	got := rainFromSamples(samples)
	sum := func(from, to int64) float64 {
		total := 0.0
		for _, r := range got {
			if r.T.Unix() > from && r.T.Unix() <= to {
				total += r.V
			}
		}
		return math.Round(total*25.4*100) / 100 // back to mm
	}
	// Before the first report: its 24-hour total, spread over the day before it.
	if mm := sum(at(9, 15, 0), at(10, 15, 0)); mm != 4.8 {
		t.Errorf("backfill = %v mm, want 4.8", mm)
	}
	if mm := sum(at(10, 15, 0), at(10, 15, 30)); mm != 1.0 {
		t.Errorf("15:00-15:30 = %v mm, want 1.0", mm)
	}
	if mm := sum(at(10, 15, 30), at(10, 16, 20)); mm != 1.2 {
		t.Errorf("across the reset = %v mm, want 1.2", mm)
	}
	if mm := sum(at(10, 17, 0), at(10, 19, 0)); mm != 0.8 { // 2 of the gap's 5 hours
		t.Errorf("17:00-19:00 = %v mm, want 0.8", mm)
	}
	if mm := sum(at(10, 22, 0), at(11, 6, 0)); mm != 0 {
		t.Errorf("after the long gap = %v mm, want 0 (unknown)", mm)
	}
}

// fakeRedis implements the few sorted-set commands addCWOPRain uses, over Upstash's REST pipeline.
func fakeRedis(t *testing.T) *httptest.Server {
	sets := map[string]map[string]float64{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/pipeline" || r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "bad request", http.StatusUnauthorized)
			return
		}
		var cmds [][]any
		json.NewDecoder(r.Body).Decode(&cmds)
		num := func(v any) float64 {
			switch x := v.(type) {
			case float64:
				return x
			case string:
				if x == "-inf" {
					return math.Inf(-1)
				} else if x == "+inf" {
					return math.Inf(1)
				}
				f, _ := strconv.ParseFloat(x, 64)
				return f
			}
			return 0
		}
		out := []map[string]any{}
		for _, c := range cmds {
			key := c[1].(string)
			if sets[key] == nil {
				sets[key] = map[string]float64{}
			}
			switch c[0] {
			case "ZADD":
				sets[key][c[3].(string)] = num(c[2])
				out = append(out, map[string]any{"result": 1})
			case "ZREMRANGEBYSCORE":
				for m, s := range sets[key] {
					if s >= num(c[2]) && s <= num(c[3]) {
						delete(sets[key], m)
					}
				}
				out = append(out, map[string]any{"result": 0})
			case "ZRANGEBYSCORE":
				members := []string{}
				for m, s := range sets[key] {
					if s >= num(c[2]) && s <= num(c[3]) {
						members = append(members, m)
					}
				}
				sort.Slice(members, func(i, j int) bool { return sets[key][members[i]] < sets[key][members[j]] })
				out = append(out, map[string]any{"result": members})
			}
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAddCWOPRain(t *testing.T) {
	loc, _ := time.LoadLocation("America/Los_Angeles")
	base := time.Date(2026, 11, 10, 9, 0, 0, 0, loc)
	report := base // the stations' latest report, moved along between runs
	mn := map[string]float64{"EW5752": 4.6, "FW4618": 5.6}
	aprs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apikey") != "test-key" || r.URL.Query().Get("what") != "wx" {
			http.Error(w, "bad key", http.StatusForbidden)
			return
		}
		entries := []any{}
		for _, name := range strings.Split(r.URL.Query().Get("name"), ",") {
			e := map[string]any{"name": name, "time": fmt.Sprint(report.Unix()), "temp": "6.7"}
			if v, ok := mn[name]; ok { // stations without a gauge send no rain fields
				e["rain_mn"], e["rain_1h"], e["rain_24h"] = fmt.Sprint(v), "0", "10.2"
			}
			entries = append(entries, e)
		}
		json.NewEncoder(w).Encode(map[string]any{"result": "ok", "entries": entries})
	}))
	t.Cleanup(aprs.Close)
	redis := fakeRedis(t)
	oldBase, oldKey, oldStore := aprsBase, aprsKey, aprsStore
	aprsBase, aprsKey, aprsStore = aprs.URL, "test-key", &redisStore{redis.URL, "test-token", http.DefaultClient}
	t.Cleanup(func() { aprsBase, aprsKey, aprsStore = oldBase, oldKey, oldStore })

	c := newNWSClient()
	run := func() []stationSeries {
		series := []stationSeries{{ID: "LVN11"}, {ID: "E5752"}, {ID: "F4618"}, {ID: "C1234"}}
		if err := c.addCWOPRain(context.Background(), series, loc, report.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		return series
	}
	total := func(p []reading) float64 {
		t := 0.0
		for _, r := range p {
			t += r.V
		}
		return math.Round(t*25.4*100) / 100
	}
	// The first run has only one report: its 24-hour total (10.2 mm) fills the day before it.
	if s := run(); total(s[1].Precip) != 10.2 || len(s[1].Precip) != 24 {
		t.Errorf("first run precip = %v mm in %d readings", total(s[1].Precip), len(s[1].Precip))
	}
	// Half an hour later EW5752 has 1.27 mm more and FW4618 none; C1234 has no gauge.
	report = base.Add(30 * time.Minute)
	mn["EW5752"] += 1.27
	s := run()
	last := s[1].Precip[len(s[1].Precip)-1]
	if total(s[1].Precip) != 11.47 || math.Abs(last.V-0.05) > 0.001 || !last.T.Equal(report) {
		t.Errorf("EW5752 precip = %v mm, last %+v", total(s[1].Precip), last)
	}
	if total(s[2].Precip) != 10.2 || s[2].Precip[len(s[2].Precip)-1].V != 0 {
		t.Errorf("FW4618 precip = %v mm", total(s[2].Precip))
	}
	if len(s[0].Precip) != 0 || len(s[3].Precip) != 0 {
		t.Errorf("non-gauge stations got rain: %+v / %+v", s[0].Precip, s[3].Precip)
	}
	// Without a key nothing is fetched.
	aprsKey = ""
	report = base.Add(time.Hour)
	if s := run(); len(s[1].Precip) != 0 {
		t.Errorf("without a key precip = %+v", s[1].Precip)
	}
}
