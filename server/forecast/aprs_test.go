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
	loc, _ := time.LoadLocation("America/Los_Angeles")
	at := func(d, h, m int) int64 { return time.Date(2026, 11, d, h, m, 0, 0, loc).Unix() }
	samples := []aprsSample{
		{T: at(9, 22, 0), Mn: 2.0, R1: 0.5},
		{T: at(9, 22, 30), Mn: 3.0, R1: 1.0}, // +1.0 mm
		{T: at(9, 23, 30), Mn: 5.0, R1: 2.0}, // +2.0 mm
		{T: at(10, 0, 20), Mn: 1.0, R1: 1.8}, // past midnight: 1.0 today + 0.8 of the last hour before it
		{T: at(10, 6, 0), Mn: 4.0, R1: 0.0},  // 5h40m gap: unknown
		{T: at(10, 6, 30), Mn: 5.27, R1: 1.27},
	}
	got := rainFromSamples(samples, loc)
	want := []float64{1.0, 2.0, 1.8, 1.27}
	if len(got) != len(want) {
		t.Fatalf("got %d readings: %+v", len(got), got)
	}
	for i, r := range got {
		if math.Abs(r.V-want[i]/25.4) > 0.001 {
			t.Errorf("reading %d = %.3f″, want %.3f″", i, r.V, want[i]/25.4)
		}
	}
	if !got[3].T.Equal(time.Unix(at(10, 6, 30), 0)) {
		t.Errorf("last reading at %v", got[3].T)
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
	// The first run only saves a report: no rain yet, since rain is the change between reports.
	if s := run(); len(s[1].Precip) != 0 {
		t.Errorf("first run precip = %+v", s[1].Precip)
	}
	// Half an hour later EW5752 has 1.27 mm more and FW4618 none; C1234 has no gauge.
	report = base.Add(30 * time.Minute)
	mn["EW5752"] += 1.27
	s := run()
	if len(s[1].Precip) != 1 || math.Abs(s[1].Precip[0].V-0.05) > 0.001 || !s[1].Precip[0].T.Equal(report) {
		t.Errorf("EW5752 precip = %+v", s[1].Precip)
	}
	if len(s[2].Precip) != 1 || s[2].Precip[0].V != 0 {
		t.Errorf("FW4618 precip = %+v", s[2].Precip)
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
