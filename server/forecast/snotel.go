package forecast

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
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
	if err := c.getAs(ctx, u, "application/json", &list); err != nil {
		return nil, err
	}
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
	for i := range found {
		wg.Add(1)
		go func(n *near) {
			defer wg.Done()
			// The service takes station-local dates; an hour of slack covers the hour before since.
			from, to := since.Add(-time.Hour).In(n.tz), now.Add(time.Hour).In(n.tz)
			u := snotelBase + "/data?stationTriplets=" + url.QueryEscape(n.id) +
				"&elements=PREC,SNWD,TOBS&duration=HOURLY" +
				"&beginDate=" + url.QueryEscape(from.Format("2006-01-02 15:04")) +
				"&endDate=" + url.QueryEscape(to.Format("2006-01-02 15:04"))
			d := snotelData{}
			if err := c.getAs(ctx, u, "application/json", &d); err != nil {
				return
			}
			parseSnotel(d, n.tz, &n.st)
		}(&found[i])
	}
	wg.Wait()

	out := make([]stationSeries, len(found))
	for i, n := range found {
		out[i] = n.st
	}
	return out, nil
}

// parseSnotel fills a station's series from an AWDB response. PREC is the water year's running
// total (0.1″ resolution), so each reading's precipitation is the rise since the one before. Across
// a gap of a few missing hours the rise counts at the reading after it rather than being lost; a
// drop (the October reset, or a gauge correction) counts as none.
func parseSnotel(d snotelData, tz *time.Location, s *stationSeries) {
	for _, st := range d {
		for _, el := range st.Data {
			prevT, prevV := time.Time{}, math.NaN()
			for _, v := range el.Values {
				if v.Value == nil {
					continue
				}
				t, err := time.ParseInLocation("2006-01-02 15:04", v.Date, tz)
				if err != nil {
					continue
				}
				switch el.StationElement.ElementCode {
				case "PREC":
					if gap := t.Sub(prevT); gap > 0 && gap <= maxSnotelGap {
						s.Precip = append(s.Precip, reading{t, math.Max(0, math.Round((*v.Value-prevV)*100)/100)})
					}
					prevT, prevV = t, *v.Value
				case "SNWD":
					s.Depth = append(s.Depth, reading{t, *v.Value})
				case "TOBS":
					s.Times, s.TempF = append(s.Times, t), append(s.TempF, *v.Value)
				}
			}
		}
	}
}
