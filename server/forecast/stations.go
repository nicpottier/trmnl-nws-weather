package forecast

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Station readings for the chart's past hours and the current temperature come from
// api.weather.gov, which carries the local networks too: the Leavenworth station (LVN11) and the
// NWAC stations at Stevens Pass. It has temperatures and wind for them, but no precipitation or
// snow depth.

// stationSeries is one station's readings, oldest first.
type stationSeries struct {
	ID          string
	Name        string
	ElevationFt float64
	DistanceKm  float64
	Times       []time.Time // when TempF was measured
	TempF       []float64
	Depth       []reading // snow depth, inches
	Precip      []reading // precipitation in the hour ending at T, inches
	Gust        []reading // wind gusts, mph
}

type reading struct {
	T time.Time
	V float64
}

const (
	stationRadiusKm    = 16.1 // 10 miles
	maxNearbyStations  = 8
	stationListMaxAge  = 24 * time.Hour
	stationListMaxPage = 20
)

type stationMeta struct {
	ID       string
	Name     string
	Lat, Lon float64
	ElevFt   float64
}

type stationListPage struct {
	Features []struct {
		Geometry struct {
			Coordinates []float64 `json:"coordinates"` // lon, lat
		} `json:"geometry"`
		Properties struct {
			StationIdentifier string   `json:"stationIdentifier"`
			Name              string   `json:"name"`
			Elevation         Quantity `json:"elevation"` // m
		} `json:"properties"`
	} `json:"features"`
	Pagination struct {
		Next string `json:"next"`
	} `json:"pagination"`
}

// stateStations lists a state's observation stations, kept for a day: the forecast point's own
// station list skips the ones that matter here (Stevens Pass' list starts at Skykomish).
func (c *nwsClient) stateStations(ctx context.Context, state string) ([]stationMeta, error) {
	c.mu.Lock()
	cached, ok := c.stations[state]
	c.mu.Unlock()
	if ok && time.Since(cached.at) < stationListMaxAge {
		return cached.list, nil
	}
	list := []stationMeta{}
	next := nwsBase + "/stations?state=" + url.QueryEscape(state) + "&limit=500"
	for page := 0; next != "" && page < stationListMaxPage; page++ {
		p := &stationListPage{}
		if err := c.get(ctx, next, p); err != nil {
			return nil, err
		}
		if len(p.Features) == 0 {
			break
		}
		for _, f := range p.Features {
			if len(f.Geometry.Coordinates) < 2 {
				continue
			}
			m := stationMeta{ID: f.Properties.StationIdentifier, Name: f.Properties.Name,
				Lon: f.Geometry.Coordinates[0], Lat: f.Geometry.Coordinates[1], ElevFt: math.NaN()}
			if e := f.Properties.Elevation.Value; e != nil {
				m.ElevFt = *e / 0.3048
			}
			list = append(list, m)
		}
		next = p.Pagination.Next
	}
	c.mu.Lock()
	c.stations[state] = cachedStations{time.Now(), list}
	c.mu.Unlock()
	return list, nil
}

type cachedStations struct {
	at   time.Time
	list []stationMeta
}

type observationList struct {
	Features []struct {
		Properties struct {
			Timestamp             time.Time `json:"timestamp"`
			Temperature           Quantity  `json:"temperature"`           // degC
			WindGust              Quantity  `json:"windGust"`              // km/h or m/s, see UnitCode
			PrecipitationLastHour Quantity  `json:"precipitationLastHour"` // mm
		} `json:"properties"`
	} `json:"features"`
}

// stationSeriesNear fetches readings since `since` from up to maxNearbyStations stations within
// stationRadiusKm of a point, nearest first.
func (c *nwsClient) stationSeriesNear(ctx context.Context, lat, lon float64, state string, since time.Time) ([]stationSeries, error) {
	if state == "" {
		return nil, fmt.Errorf("no state for this point")
	}
	all, err := c.stateStations(ctx, state)
	if err != nil {
		return nil, err
	}
	near := []stationSeries{}
	for _, m := range all {
		if d := distanceKm(lat, lon, m.Lat, m.Lon); d <= stationRadiusKm {
			near = append(near, stationSeries{ID: m.ID, Name: m.Name, ElevationFt: m.ElevFt, DistanceKm: d})
		}
	}
	sort.SliceStable(near, func(i, j int) bool { return near[i].DistanceKm < near[j].DistanceKm })
	near = near[:min(len(near), maxNearbyStations)]

	var wg sync.WaitGroup
	for i := range near {
		wg.Add(1)
		go func(s *stationSeries) {
			defer wg.Done()
			obs := &observationList{}
			u := nwsBase + "/stations/" + url.PathEscape(s.ID) + "/observations?start=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
			if err := c.get(ctx, u, obs); err != nil {
				return
			}
			for k := len(obs.Features) - 1; k >= 0; k-- { // newest first in the feed
				p := obs.Features[k].Properties
				if v := p.Temperature.Value; v != nil {
					s.Times = append(s.Times, p.Timestamp)
					s.TempF = append(s.TempF, *v*9/5+32)
				}
				if v := p.WindGust.Value; v != nil {
					mph := *v / kmhPerMph
					if strings.HasSuffix(p.WindGust.UnitCode, "m_s-1") {
						mph = *v * 2.237
					}
					s.Gust = append(s.Gust, reading{p.Timestamp, mph})
				}
				if v := p.PrecipitationLastHour.Value; v != nil {
					s.Precip = append(s.Precip, reading{p.Timestamp, *v / 25.4})
				}
			}
		}(&near[i])
	}
	wg.Wait()
	return near, nil
}

func distanceKm(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180
	dLat, dLon := (lat2-lat1)*rad, (lon2-lon1)*rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) + math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 6371 * 2 * math.Asin(math.Sqrt(a))
}

// pickStation returns the nearest station whose elevation is within maxElevationGapM of the point
// (when known) and whose latest reading is at most maxAge old, or nil.
func pickStation(series []stationSeries, pointElevM float64, now time.Time, maxAge time.Duration) *stationSeries {
	for i := range series {
		s := &series[i]
		if len(s.Times) == 0 || now.Sub(s.Times[len(s.Times)-1]) > maxAge || !s.nearElevation(pointElevM) {
			continue
		}
		return s
	}
	return nil
}

// nearElevation reports whether the station is within maxElevationGapM of the point; an unknown
// (NaN) point elevation never rules a station out.
func (s *stationSeries) nearElevation(pointElevM float64) bool {
	return !(math.Abs(s.ElevationFt*0.3048-pointElevM) > maxElevationGapM)
}

// latest returns the station's most recent reading as an Observation.
func (s *stationSeries) latest() *Observation {
	n := len(s.Times) - 1
	return &Observation{Station: s.ID, TempF: s.TempF[n], ObservedAt: s.Times[n]}
}

// hourly averages the readings into one value per hour, keyed by the hour's start in Unix
// seconds, in °F.
func (s *stationSeries) hourly() map[int64]float64 {
	sums, counts := map[int64]float64{}, map[int64]int{}
	for i, t := range s.Times {
		h := t.Truncate(time.Hour).Unix()
		sums[h] += s.TempF[i]
		counts[h]++
	}
	out := map[int64]float64{}
	for h, sum := range sums {
		out[h] = sum / float64(counts[h])
	}
	return out
}
