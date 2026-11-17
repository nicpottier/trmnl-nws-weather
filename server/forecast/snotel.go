package forecast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// NRCS SNOTEL stations measure precipitation and snow depth hourly, through USDA's open AWDB
// service (no key). Stevens Pass has one (site 791, 3,940 ft) beside NWAC's Schmidt Haus station;
// api.weather.gov carries temperatures but no precipitation or depth for that area, so SNOTEL
// supplies the measured rain and snow bars and the fresh-snow totals.
var snotelBase = "https://wcc.sc.egov.usda.gov/awdbRestApi/services/v1"

const maxSnotelGap = 6 * time.Hour

type snotelStation struct {
	Triplet   string  `json:"stationTriplet"`
	Name      string  `json:"name"`
	Elevation float64 `json:"elevation"` // ft
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	TimeZone  float64 `json:"dataTimeZone"` // hours from UTC; the data's timestamps are in this fixed offset
}

type cachedSnotel struct {
	at   time.Time
	list []snotelStation
}

type snotelData []struct {
	Triplet string `json:"stationTriplet"`
	Data    []struct {
		StationElement struct {
			ElementCode string `json:"elementCode"`
		} `json:"stationElement"`
		Values []struct {
			Date  string   `json:"date"` // "2026-11-10 08:00", station standard time
			Value *float64 `json:"value"`
		} `json:"values"`
	} `json:"data"`
}

// snotelStations lists a state's active SNOTEL stations, kept for a day.
func (c *nwsClient) snotelStations(ctx context.Context, state string) ([]snotelStation, error) {
	c.mu.Lock()
	cached, ok := c.snotel[state]
	c.mu.Unlock()
	if ok && time.Since(cached.at) < stationListMaxAge {
		return cached.list, nil
	}
	list := []snotelStation{}
	u := snotelBase + "/stations?stationTriplets=" + url.QueryEscape("*:"+state+":SNTL") +
		"&activeOnly=true&returnForecastPointMetadata=false&returnReservoirMetadata=false&returnStationElements=false"
	key := "snotel:stations:" + state
	if err := c.getAs(ctx, u, "application/json", &list); err != nil {
		// USDA's service is down now and then: use the list saved last time, retrying in an hour.
		saved, ok := loadJSON[[]snotelStation](ctx, key)
		if !ok {
			return nil, err
		}
		c.mu.Lock()
		c.snotel[state] = cachedSnotel{time.Now().Add(time.Hour - stationListMaxAge), saved}
		c.mu.Unlock()
		return saved, nil
	}
	saveJSON(ctx, key, list, 30*24*time.Hour)
	c.mu.Lock()
	c.snotel[state] = cachedSnotel{time.Now(), list}
	c.mu.Unlock()
	return list, nil
}

// snotelSeriesNear fetches hourly precipitation, snow depth and temperature since `since` from the
// SNOTEL stations within stationRadiusKm of a point.
func (c *nwsClient) snotelSeriesNear(ctx context.Context, lat, lon float64, state string, since, now time.Time) ([]stationSeries, error) {
	if state == "" {
		return nil, fmt.Errorf("no state for this point")
	}
	all, err := c.snotelStations(ctx, state)
	if err != nil {
		return nil, err
	}
	type near struct {
		st stationSeries
		tz *time.Location
		id string
	}
	found := []near{}
	for _, s := range all {
		if d := distanceKm(lat, lon, s.Latitude, s.Longitude); d <= stationRadiusKm {
			found = append(found, near{
				stationSeries{ID: s.Triplet, Name: s.Name, ElevationFt: s.Elevation, DistanceKm: d},
				time.FixedZone("", int(s.TimeZone*3600)), s.Triplet,
			})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].st.DistanceKm < found[j].st.DistanceKm })

	var wg sync.WaitGroup
	errs := make([]error, len(found))
	for i := range found {
		wg.Add(1)
		go func(i int, n *near) {
			defer wg.Done()
			// The service takes station-local dates; an hour of slack covers the hour before since.
			from, to := since.Add(-time.Hour).In(n.tz), now.Add(time.Hour).In(n.tz)
			u := snotelBase + "/data?stationTriplets=" + url.QueryEscape(n.id) +
				"&elements=PREC,SNWD,TOBS&duration=HOURLY" +
				"&beginDate=" + url.QueryEscape(from.Format("2006-01-02 15:04")) +
				"&endDate=" + url.QueryEscape(to.Format("2006-01-02 15:04"))
			d := snotelData{}
			fresh := map[int64]snotelRecord{}
			if errs[i] = c.getAs(ctx, u, "application/json", &d); errs[i] == nil {
				fresh = snotelRecords(d, n.tz)
			}
			// Merge with the readings saved by earlier runs, so an outage only costs its own hours.
			recs := mergeSnotelHistory(ctx, n.id, fresh, now)
			seriesFromSnotel(recs, &n.st)
		}(i, &found[i])
	}
	wg.Wait()

	out := make([]stationSeries, len(found))
	for i, n := range found {
		out[i] = n.st
	}
	return out, errors.Join(errs...)
}

// snotelRecord is one hour at a SNOTEL station; the precipitation is the water year's running total.
type snotelRecord struct {
	T     int64    `json:"t"`
	Prec  *float64 `json:"p,omitempty"`
	Depth *float64 `json:"d,omitempty"`
	Temp  *float64 `json:"f,omitempty"`
}

// snotelRecords collects an AWDB response into one record per hour, keyed by Unix time.
func snotelRecords(d snotelData, tz *time.Location) map[int64]snotelRecord {
	out := map[int64]snotelRecord{}
	for _, st := range d {
		for _, el := range st.Data {
			for _, v := range el.Values {
				if v.Value == nil {
					continue
				}
				t, err := time.ParseInLocation("2006-01-02 15:04", v.Date, tz)
				if err != nil {
					continue
				}
				r := out[t.Unix()]
				r.T = t.Unix()
				val := *v.Value
				switch el.StationElement.ElementCode {
				case "PREC":
					r.Prec = &val
				case "SNWD":
					r.Depth = &val
				case "TOBS":
					r.Temp = &val
				}
				out[t.Unix()] = r
			}
		}
	}
	return out
}

// snotelKeep is how long SNOTEL readings are saved.
const snotelKeep = 72 * time.Hour

// mergeSnotelHistory merges fresh readings into the station's saved ones (fresh values win, field by
// field), saves the result and drops readings older than snotelKeep. Without a store it returns the
// fresh readings.
func mergeSnotelHistory(ctx context.Context, id string, fresh map[int64]snotelRecord, now time.Time) []snotelRecord {
	merged := map[int64]snotelRecord{}
	key := "snotel:" + id
	var stale []any
	if stateStore != nil {
		if res, err := stateStore.pipeline(ctx, []any{"HGETALL", key}); err == nil && len(res) == 1 {
			var flat []string
			if json.Unmarshal(res[0], &flat) == nil {
				for i := 0; i+1 < len(flat); i += 2 {
					r := snotelRecord{}
					if json.Unmarshal([]byte(flat[i+1]), &r) != nil {
						continue
					}
					if time.Unix(r.T, 0).Before(now.Add(-snotelKeep)) {
						stale = append(stale, flat[i])
						continue
					}
					merged[r.T] = r
				}
			}
		}
	}
	changed := []any{}
	for t, f := range fresh {
		if time.Unix(t, 0).Before(now.Add(-snotelKeep)) {
			continue
		}
		r := merged[t]
		r.T = t
		if f.Prec != nil {
			r.Prec = f.Prec
		}
		if f.Depth != nil {
			r.Depth = f.Depth
		}
		if f.Temp != nil {
			r.Temp = f.Temp
		}
		if old, ok := merged[t]; !ok || !sameRecord(old, r) {
			b, _ := json.Marshal(r)
			changed = append(changed, strconv.FormatInt(t, 10), string(b))
		}
		merged[t] = r
	}
	if stateStore != nil && (len(changed) > 0 || len(stale) > 0) {
		cmds := [][]any{}
		if len(changed) > 0 {
			cmds = append(cmds, append([]any{"HSET", key}, changed...))
		}
		if len(stale) > 0 {
			cmds = append(cmds, append([]any{"HDEL", key}, stale...))
		}
		stateStore.pipeline(ctx, cmds...) // best effort: next run tries again
	}
	out := make([]snotelRecord, 0, len(merged))
	for _, r := range merged {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out
}

func sameRecord(a, b snotelRecord) bool {
	eq := func(x, y *float64) bool { return (x == nil) == (y == nil) && (x == nil || *x == *y) }
	return a.T == b.T && eq(a.Prec, b.Prec) && eq(a.Depth, b.Depth) && eq(a.Temp, b.Temp)
}

// parseSnotel fills a station's series from an AWDB response (see seriesFromSnotel).
func parseSnotel(d snotelData, tz *time.Location, s *stationSeries) {
	recs := []snotelRecord{}
	for _, r := range snotelRecords(d, tz) {
		recs = append(recs, r)
	}
	sort.Slice(recs, func(i, j int) bool { return recs[i].T < recs[j].T })
	seriesFromSnotel(recs, s)
}

// seriesFromSnotel fills a station's series from hourly records, oldest first. PREC is the water
// year's running total (0.1″ resolution), so each reading's precipitation is the rise since the one
// before. Across a gap of a few missing hours the rise counts at the reading after it rather than
// being lost; a drop (the October reset, or a gauge correction) counts as none.
func seriesFromSnotel(recs []snotelRecord, s *stationSeries) {
	prevT, prevV := time.Time{}, math.NaN()
	for _, r := range recs {
		t := time.Unix(r.T, 0)
		if r.Prec != nil {
			if gap := t.Sub(prevT); gap > 0 && gap <= maxSnotelGap {
				s.Precip = append(s.Precip, reading{t, math.Max(0, math.Round((*r.Prec-prevV)*100)/100)})
			}
			prevT, prevV = t, *r.Prec
		}
		if r.Depth != nil {
			s.Depth = append(s.Depth, reading{t, *r.Depth})
		}
		if r.Temp != nil {
			s.Times, s.TempF = append(s.Times, t), append(s.TempF, *r.Temp)
		}
	}
}
