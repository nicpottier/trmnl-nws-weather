package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	nwsBase      = "https://api.weather.gov"
	forecastBase = "https://forecast.weather.gov"
)

// NWS asks every client to identify itself with a contact in the User-Agent.
var userAgent = envOr("NWS_USER_AGENT", "trmnl-nws-weather (https://github.com/nicpottier/trmnl-nws-weather)")

type Points struct {
	Properties struct {
		GridID              string `json:"gridId"`
		GridX               int    `json:"gridX"`
		GridY               int    `json:"gridY"`
		Forecast            string `json:"forecast"`
		ForecastHourly      string `json:"forecastHourly"`
		ObservationStations string `json:"observationStations"`
		ForecastGridData    string `json:"forecastGridData"`
		TimeZone            string `json:"timeZone"`
		RelativeLocation    struct {
			Properties struct {
				City  string `json:"city"`
				State string `json:"state"`
			} `json:"properties"`
		} `json:"relativeLocation"`
	} `json:"properties"`
}

type Quantity struct {
	Value *float64 `json:"value"`
}

func (q Quantity) Or(def float64) float64 {
	if q.Value == nil {
		return def
	}
	return *q.Value
}

type Period struct {
	Number                     int       `json:"number"`
	Name                       string    `json:"name"`
	StartTime                  time.Time `json:"startTime"`
	EndTime                    time.Time `json:"endTime"`
	IsDaytime                  bool      `json:"isDaytime"`
	Temperature                int       `json:"temperature"`
	ProbabilityOfPrecipitation Quantity  `json:"probabilityOfPrecipitation"`
	RelativeHumidity           Quantity  `json:"relativeHumidity"`
	WindSpeed                  string    `json:"windSpeed"`
	WindDirection              string    `json:"windDirection"`
	ShortForecast              string    `json:"shortForecast"`
	DetailedForecast           string    `json:"detailedForecast"`
}

func (p Period) Pop() int { return int(p.ProbabilityOfPrecipitation.Or(0)) }

type Forecast struct {
	Properties struct {
		Periods []Period `json:"periods"`
	} `json:"properties"`
}

type Alerts struct {
	Features []struct {
		Properties Alert `json:"properties"`
	} `json:"features"`
}

type Alert struct {
	Event       string `json:"event"`
	Severity    string `json:"severity"`
	Headline    string `json:"headline"`
	Status      string `json:"status"`
	MessageType string `json:"messageType"`
}

type nwsClient struct {
	http *http.Client

	// Grid lookups never change for a location, so warm Lambdas skip them.
	mu     sync.Mutex
	points map[string]*Points
}

func newNWSClient() *nwsClient {
	return &nwsClient{http: &http.Client{Timeout: 8 * time.Second}, points: map[string]*Points{}}
}

func (c *nwsClient) get(ctx context.Context, url string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/geo+json")

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				// NWS intermittently returns 500s for gridpoint forecasts; one retry usually clears it.
				lastErr = fmt.Errorf("GET %s: %s", url, resp.Status)
				return
			}
			lastErr = json.NewDecoder(resp.Body).Decode(v)
		}()
		if lastErr == nil {
			return nil
		}
	}
	return lastErr
}

func (c *nwsClient) Points(ctx context.Context, lat, lon float64) (*Points, error) {
	key := fmt.Sprintf("%.4f,%.4f", lat, lon)
	c.mu.Lock()
	p, ok := c.points[key]
	c.mu.Unlock()
	if ok {
		return p, nil
	}
	p = &Points{}
	if err := c.get(ctx, nwsBase+"/points/"+key, p); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.points[key] = p
	c.mu.Unlock()
	return p, nil
}

func (c *nwsClient) Forecast(ctx context.Context, url, units string) ([]Period, error) {
	f := &Forecast{}
	if err := c.get(ctx, url+"?units="+units, f); err != nil {
		return nil, err
	}
	return f.Properties.Periods, nil
}

func (c *nwsClient) Alerts(ctx context.Context, lat, lon float64) ([]Alert, error) {
	a := &Alerts{}
	if err := c.get(ctx, fmt.Sprintf("%s/alerts/active?point=%.4f,%.4f", nwsBase, lat, lon), a); err != nil {
		return nil, err
	}
	alerts := []Alert{}
	for _, f := range a.Features {
		if f.Properties.Status == "Actual" && f.Properties.MessageType != "Cancel" {
			alerts = append(alerts, f.Properties)
		}
	}
	return alerts, nil
}

// Observation is a measured current temperature, always in °F.
type Observation struct {
	Station    string
	TempF      float64
	Humidity   *float64
	ObservedAt time.Time
}

const (
	// maxObservationAge is how old a reading can be before we fall back to the hourly forecast.
	maxObservationAge = 2 * time.Hour
	// maxElevationGapM keeps a valley station from standing in for a mountain pass (or vice versa).
	maxElevationGapM = 300.0
)

// CurrentObservation finds the freshest nearby measured temperature.
//
// api.weather.gov's observation feed often lags or drops the cooperative stations closest to a
// point (Leavenworth's LVN11 can be days behind there), while forecast.weather.gov's own
// point-forecast JSON — the source of the "current conditions" box on its MapClick page —
// carries the latest reading. So try that first, then the gridpoint's stations in distance order.
//
// pointElevM is the forecast point's elevation in meters, or NaN if unknown.
func (c *nwsClient) CurrentObservation(ctx context.Context, lat, lon float64, p *Points, pointElevM float64, loc *time.Location, now time.Time) (*Observation, error) {
	if obs, err := c.mapClickObservation(ctx, lat, lon, loc, now); err == nil && now.Sub(obs.ObservedAt) <= maxObservationAge {
		return obs, nil
	}
	return c.stationObservation(ctx, p, pointElevM, now)
}

// looseNumber accepts MapClick's numbers, which arrive as JSON strings, numbers or "NA".
type looseNumber string

func (n *looseNumber) UnmarshalJSON(b []byte) error {
	*n = looseNumber(strings.Trim(string(b), `"`))
	return nil
}

func (n looseNumber) Float64() (float64, error) { return strconv.ParseFloat(string(n), 64) }

type mapClick struct {
	Location struct {
		Elevation looseNumber `json:"elevation"` // feet
	} `json:"location"`
	CurrentObservation struct {
		Elev looseNumber `json:"elev"` // feet
		ID   string      `json:"id"`
		Date string      `json:"Date"`
		Temp string      `json:"Temp"`
		Relh string      `json:"Relh"`
	} `json:"currentobservation"`
}

func (c *nwsClient) mapClickObservation(ctx context.Context, lat, lon float64, loc *time.Location, now time.Time) (*Observation, error) {
	m := &mapClick{}
	u := fmt.Sprintf("%s/MapClick.php?lat=%.4f&lon=%.4f&FcstType=json", forecastBase, lat, lon)
	if err := c.get(ctx, u, m); err != nil {
		return nil, err
	}
	co := m.CurrentObservation
	pointFt, err1 := m.Location.Elevation.Float64()
	stationFt, err2 := co.Elev.Float64()

	if err1 == nil && err2 == nil && math.Abs(pointFt-stationFt)*0.3048 > maxElevationGapM {
		return nil, fmt.Errorf("MapClick station %s is at %.0f ft, the point at %.0f ft", co.ID, stationFt, pointFt)
	}
	temp, err := strconv.ParseFloat(co.Temp, 64)
	if err != nil {
		return nil, fmt.Errorf("no temperature in MapClick observation: %q", co.Temp)
	}
	at, err := parseMapClickDate(co.Date, loc, now)
	if err != nil {
		return nil, err
	}
	obs := &Observation{Station: co.ID, TempF: temp, ObservedAt: at}
	if rh, err := strconv.ParseFloat(co.Relh, 64); err == nil {
		obs.Humidity = &rh
	}
	return obs, nil
}

// parseMapClickDate parses "25 Oct 07:00 AM PDT", which has no year, as the most recent such time.
func parseMapClickDate(s string, loc *time.Location, now time.Time) (time.Time, error) {
	fields := strings.Fields(s)
	if len(fields) < 4 {
		return time.Time{}, fmt.Errorf("unexpected MapClick date %q", s)
	}
	local := now.In(loc)
	t, err := time.ParseInLocation("2 Jan 2006 03:04 PM", fmt.Sprintf("%s %s %d %s %s", fields[0], fields[1], local.Year(), fields[2], fields[3]), loc)
	if err != nil {
		return time.Time{}, err
	}
	if t.After(local.Add(24 * time.Hour)) {
		t = t.AddDate(-1, 0, 0) // a late-December reading seen in early January
	}
	return t, nil
}

type stationList struct {
	Features []struct {
		Properties struct {
			StationIdentifier string   `json:"stationIdentifier"`
			Elevation         Quantity `json:"elevation"`
		} `json:"properties"`
	} `json:"features"`
}

type stationObs struct {
	Properties struct {
		Timestamp        time.Time `json:"timestamp"`
		Temperature      Quantity  `json:"temperature"`
		RelativeHumidity Quantity  `json:"relativeHumidity"`
	} `json:"properties"`
}

// stationObservation returns the closest of the gridpoint's first few stations with a fresh reading.
func (c *nwsClient) stationObservation(ctx context.Context, p *Points, pointElevM float64, now time.Time) (*Observation, error) {
	if p.Properties.ObservationStations == "" {
		return nil, fmt.Errorf("no observation stations for this point")
	}
	list := &stationList{}
	if err := c.get(ctx, p.Properties.ObservationStations+"?limit=5", list); err != nil {
		return nil, err
	}

	results := make([]*Observation, len(list.Features))
	var wg sync.WaitGroup
	for i, f := range list.Features {
		if elev := f.Properties.Elevation.Value; elev != nil && math.Abs(*elev-pointElevM) > maxElevationGapM {
			continue // NaN (unknown point elevation) never compares greater, so every station passes
		}
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			o := &stationObs{}
			if err := c.get(ctx, nwsBase+"/stations/"+url.PathEscape(id)+"/observations/latest", o); err != nil {
				return
			}
			op := o.Properties
			if op.Temperature.Value == nil || now.Sub(op.Timestamp) > maxObservationAge {
				return
			}
			obs := &Observation{Station: id, TempF: *op.Temperature.Value*9/5 + 32, ObservedAt: op.Timestamp, Humidity: op.RelativeHumidity.Value}
			results[i] = obs
		}(i, f.Properties.StationIdentifier)
	}
	wg.Wait()

	for _, obs := range results { // stations come back nearest first
		if obs != nil {
			return obs, nil
		}
	}
	return nil, fmt.Errorf("no station has a reading from the last %s", maxObservationAge)
}

func fToUnits(f float64, units string) int {
	if units == "si" {
		return int(math.Round((f - 32) * 5 / 9))
	}
	return int(math.Round(f))
}
