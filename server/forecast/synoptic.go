package forecast

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"sort"
	"time"
)

// Synoptic Data (MesoWest) aggregates station networks NWS doesn't serve fresh, including the
// Leavenworth (LVN11) and Stevens Pass ski area stations. With a token in SYNOPTIC_TOKEN, the
// service uses their readings for the current temperature, the chart's hours before now
// (temperature, rain and snow), and the fresh-snow totals.
var (
	synopticBase  = "https://api.synopticdata.com/v2"
	synopticToken = os.Getenv("SYNOPTIC_TOKEN")
)

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

type synopticResponse struct {
	Summary struct {
		ResponseCode    int    `json:"RESPONSE_CODE"`
		ResponseMessage string `json:"RESPONSE_MESSAGE"`
	} `json:"SUMMARY"`
	Station []struct {
		STID         string      `json:"STID"`
		Name         string      `json:"NAME"`
		Elevation    looseNumber `json:"ELEVATION"` // feet
		Distance     looseNumber `json:"DISTANCE"`  // miles
		Observations struct {
			DateTime  []time.Time `json:"date_time"`
			AirTemp   []*float64  `json:"air_temp_set_1"`
			SnowDepth []*float64  `json:"snow_depth_set_1"`
			Precip    []*float64  `json:"precip_accum_one_hour_set_1"`
			Gust      []*float64  `json:"wind_gust_set_1"` // knots
		} `json:"OBSERVATIONS"`
	} `json:"STATION"`
}

// synopticSeries fetches temperature, snow depth and hourly precipitation since `since` for up
// to 8 stations within 10 miles of a point, nearest first. It returns nothing (and no error) when no token is configured.
func (c *nwsClient) synopticSeries(ctx context.Context, lat, lon float64, since, now time.Time) ([]stationSeries, error) {
	if synopticToken == "" {
		return nil, nil
	}
	q := url.Values{
		"token":      {synopticToken},
		"radius":     {fmt.Sprintf("%.4f,%.4f,10", lat, lon)},
		"limit":      {"8"},
		"vars":       {"air_temp,snow_depth,precip_accum_one_hour,wind_gust"},
		"units":      {"english"},
		"obtimezone": {"utc"},
		"recent":     {fmt.Sprint(int(math.Ceil(now.Sub(since).Minutes())))},
	}
	r := &synopticResponse{}
	if err := c.get(ctx, synopticBase+"/stations/timeseries?"+q.Encode(), r); err != nil {
		return nil, err
	}
	if r.Summary.ResponseCode != 1 {
		return nil, fmt.Errorf("synoptic: %s", r.Summary.ResponseMessage)
	}

	out := []stationSeries{}
	for _, s := range r.Station {
		ss := stationSeries{ID: s.STID, Name: s.Name}
		ss.ElevationFt, _ = s.Elevation.Float64()
		if d, err := s.Distance.Float64(); err == nil {
			ss.DistanceKm = d * 1.609
		}
		o := s.Observations
		at := func(vals []*float64, i int) (float64, bool) {
			if i < len(vals) && vals[i] != nil {
				return *vals[i], true
			}
			return 0, false
		}
		for i, t := range o.DateTime {
			if v, ok := at(o.AirTemp, i); ok {
				ss.Times = append(ss.Times, t)
				ss.TempF = append(ss.TempF, v)
			}
			if v, ok := at(o.SnowDepth, i); ok {
				ss.Depth = append(ss.Depth, reading{t, v})
			}
			if v, ok := at(o.Precip, i); ok {
				ss.Precip = append(ss.Precip, reading{t, v})
			}
			if v, ok := at(o.Gust, i); ok {
				ss.Gust = append(ss.Gust, reading{t, v * 1.151}) // knots -> mph
			}
		}
		out = append(out, ss)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].DistanceKm < out[j].DistanceKm })
	return out, nil
}

// pickStation returns the nearest station whose elevation is within maxElevationGapM of the
// point (when known) and that has reported within maxObservationAge, or nil.
func pickStation(series []stationSeries, pointElevM float64, now time.Time) *stationSeries {
	for i := range series {
		s := &series[i]
		if len(s.Times) == 0 || now.Sub(s.Times[len(s.Times)-1]) > maxObservationAge || !s.nearElevation(pointElevM) {
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
