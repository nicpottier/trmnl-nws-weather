package forecast

import (
	"context"
	"encoding/json"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Citizen Weather Observer Program (CWOP) stations are home weather stations whose reports reach
// NOAA through the amateur radio network. api.weather.gov lists them (Leavenworth has EW5752 and
// FW4618 nearby) with temperatures and wind but without their rain gauges. aprs.fi has the rain,
// but only each station's latest report: rain in the last hour, the last 24 hours and since local
// midnight. So every run saves those reports in Redis (store.go), and the hourly rain is rebuilt
// from the saved history. It needs APRS_FI_KEY (a free aprs.fi API key) and a Redis store; without
// either, CWOP stations simply have no rain.
var (
	aprsBase   = "https://api.aprs.fi/api/get"
	aprsKey    = os.Getenv("APRS_FI_KEY")
	aprsStore  = newRedisStore()
	aprsKeep   = 72 * time.Hour // history kept per station
	aprsMaxGap = 3 * time.Hour  // longer gaps between saved reports are left unknown
)

// cwopID matches api.weather.gov's IDs for CWOP stations: "E5752" is callsign EW5752.
var cwopID = regexp.MustCompile(`^([A-G])(\d{4})$`)

func cwopCall(nwsID string) string {
	m := cwopID.FindStringSubmatch(nwsID)
	if m == nil {
		return ""
	}
	return m[1] + "W" + m[2]
}

// aprsSample is one saved report. Rain is in millimeters, as aprs.fi gives it.
type aprsSample struct {
	T   int64   `json:"t"`   // the station's report time, Unix seconds
	Mn  float64 `json:"mn"`  // since local midnight
	R1  float64 `json:"r1"`  // last hour
	R24 float64 `json:"r24"` // last 24 hours
}

type aprsResponse struct {
	Result  string `json:"result"`
	Entries []struct {
		Name   string `json:"name"`
		Time   string `json:"time"`
		Rain1h string `json:"rain_1h"`
		Rain24 string `json:"rain_24h"`
		RainMn string `json:"rain_mn"`
	} `json:"entries"`
}

// addCWOPRain gives the CWOP stations among `series` their rain: it fetches their latest reports,
// saves them, and fills each station's Precip from its saved history.
func (c *nwsClient) addCWOPRain(ctx context.Context, series []stationSeries, loc *time.Location, now time.Time) error {
	if aprsKey == "" || aprsStore == nil {
		return nil
	}
	index := map[string]int{}
	calls := []string{}
	for i, s := range series {
		if call := cwopCall(s.ID); call != "" {
			index[call] = i
			calls = append(calls, call)
		}
	}
	if len(calls) == 0 {
		return nil
	}

	r := &aprsResponse{}
	u := aprsBase + "?what=wx&format=json&name=" + url.QueryEscape(strings.Join(calls, ",")) + "&apikey=" + url.QueryEscape(aprsKey)
	if err := c.getAs(ctx, u, "application/json", r); err != nil {
		return err
	}

	// Save each new report, drop old ones, and read back the history: one Redis round trip.
	oldest := now.Add(-aprsKeep).Unix()
	cmds := [][]any{}
	for _, call := range calls {
		key := "aprs:" + call
		for _, e := range r.Entries {
			if e.Name != call {
				continue
			}
			s, ok := parseAPRS(e.Time, e.RainMn, e.Rain1h, e.Rain24)
			if !ok {
				continue
			}
			member, _ := json.Marshal(s)
			cmds = append(cmds, []any{"ZADD", key, s.T, string(member)})
		}
		cmds = append(cmds, []any{"ZREMRANGEBYSCORE", key, "-inf", oldest - 1})
		cmds = append(cmds, []any{"ZRANGEBYSCORE", key, oldest, "+inf"})
	}
	results, err := aprsStore.pipeline(ctx, cmds...)
	if err != nil {
		return err
	}

	k := 0
	for _, call := range calls {
		for k < len(cmds) && cmds[k][0] != "ZRANGEBYSCORE" {
			k++
		}
		var members []string
		if k < len(results) && json.Unmarshal(results[k], &members) == nil {
			samples := []aprsSample{}
			for _, m := range members {
				s := aprsSample{}
				if json.Unmarshal([]byte(m), &s) == nil {
					samples = append(samples, s)
				}
			}
			series[index[call]].Precip = rainFromSamples(samples, loc)
		}
		k++
	}
	return nil
}

// parseAPRS reads a report's time and rain totals; a station without a rain gauge has none.
func parseAPRS(t, mn, r1, r24 string) (aprsSample, bool) {
	ts, err := strconv.ParseInt(t, 10, 64)
	if err != nil || mn == "" {
		return aprsSample{}, false
	}
	f := func(s string) float64 {
		v, _ := strconv.ParseFloat(s, 64)
		return v
	}
	return aprsSample{T: ts, Mn: f(mn), R1: f(r1), R24: f(r24)}, true
}

// rainFromSamples turns saved reports into rain readings, in inches, each for the time since the
// report before. Within a day that's the rise in the since-midnight total. Across midnight the total
// restarts, so it's the new day's total plus, when the reports are an hour or less apart, whatever
// of the last hour's rain fell before midnight. Gaps longer than aprsMaxGap are left unknown.
func rainFromSamples(samples []aprsSample, loc *time.Location) []reading {
	sort.Slice(samples, func(i, j int) bool { return samples[i].T < samples[j].T })
	out := []reading{}
	for i := 1; i < len(samples); i++ {
		a, b := samples[i-1], samples[i]
		ta, tb := time.Unix(a.T, 0).In(loc), time.Unix(b.T, 0).In(loc)
		gap := tb.Sub(ta)
		if gap <= 0 || gap > aprsMaxGap {
			continue
		}
		mm := 0.0
		if sameDay(ta, tb) {
			mm = math.Max(0, b.Mn-a.Mn)
		} else {
			mm = b.Mn
			if gap <= time.Hour {
				mm += math.Max(0, b.R1-b.Mn)
			}
		}
		out = append(out, reading{tb, math.Round(mm/25.4*1000) / 1000})
	}
	return out
}
