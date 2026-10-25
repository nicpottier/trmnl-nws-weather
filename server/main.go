// Command trmnl-nws-weather serves a compact NWS forecast for the TRMNL "NWS Weather Glance" plugin.
//
// It runs as an AWS Lambda behind a Function URL when AWS_LAMBDA_RUNTIME_API is set,
// and as a plain HTTP server on $PORT (default 8080) otherwise.
//
//	GET /?locations=Leavenworth,%20WA|47.5962,-120.6615;Stevens%20Pass|47.7448,-121.0890&units=us
//	GET /?lat_lon=47.5962,-120.6615&name=Leavenworth,%20WA
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
)

var nws = newNWSClient()

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func parseLatLon(s string) (float64, float64, error) {
	latS, lonS, ok := strings.Cut(s, ",")
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(latS), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(lonS), 64)
	if !ok || err1 != nil || err2 != nil {
		return 0, 0, &httpError{http.StatusBadRequest, fmt.Sprintf("%q must look like 47.5962,-120.6615", s)}
	}
	return lat, lon, nil
}

// parseRequest returns one Options per requested location, from either
//
//	locations=Leavenworth, WA | 47.5962,-120.6615;Stevens Pass | 47.7448,-121.0890
//
// (entries separated by ";" or newlines, the name optional) or the single-location
// lat_lon=47.5962,-120.6615&name=Leavenworth, WA. multi reports which form was used.
func parseRequest(q url.Values) (opts []Options, multi bool, err error) {
	// Optional shared secret so a public Function URL isn't an open NWS proxy.
	if token := os.Getenv("ACCESS_TOKEN"); token != "" &&
		subtle.ConstantTimeCompare([]byte(q.Get("token")), []byte(token)) != 1 {
		return nil, false, &httpError{http.StatusUnauthorized, "invalid token"}
	}

	base := Options{Units: "us", Days: 7, Bars: "auto"}
	switch strings.ToLower(q.Get("units")) {
	case "si", "c", "celsius", "metric":
		base.Units = "si"
	}
	if days, err := strconv.Atoi(q.Get("days")); err == nil && days >= 1 && days <= 7 {
		base.Days = days
	}
	switch b := strings.ToLower(q.Get("bars")); b {
	case "snow", "precip":
		base.Bars = b
	}

	if locs := q.Get("locations"); locs != "" {
		entries := strings.FieldsFunc(locs, func(r rune) bool { return r == ';' || r == '\n' })
		for _, entry := range entries {
			name, coords, ok := strings.Cut(entry, "|")
			if !ok {
				name, coords = "", entry
			}
			if strings.TrimSpace(coords) == "" {
				continue
			}
			o := base
			if o.Lat, o.Lon, err = parseLatLon(coords); err != nil {
				return nil, true, err
			}
			o.Name = strings.TrimSpace(name)
			opts = append(opts, o)
		}
		if len(opts) == 0 || len(opts) > 4 {
			return nil, true, &httpError{http.StatusBadRequest, "locations must list 1 to 4 locations"}
		}
		return opts, true, nil
	}

	latLon := q.Get("lat_lon")
	if latLon == "" && q.Get("lat") != "" {
		latLon = q.Get("lat") + "," + q.Get("lon")
	}
	o := base
	if o.Lat, o.Lon, err = parseLatLon(latLon); err != nil {
		return nil, false, err
	}
	o.Name = strings.TrimSpace(q.Get("name"))
	return []Options{o}, false, nil
}

// LocationError stands in for a location whose forecast couldn't be built.
type LocationError struct {
	Location string `json:"location"`
	Error    string `json:"error"`
}

// serve returns the status and JSON body for a request's query string.
func serve(ctx context.Context, q url.Values) (int, []byte) {
	opts, multi, err := parseRequest(q)
	if err != nil {
		return errorResponse(err)
	}
	now := time.Now()

	if !multi {
		g, err := BuildGlance(ctx, nws, opts[0], now)
		if err != nil {
			return errorResponse(err)
		}
		body, _ := json.Marshal(g)
		return http.StatusOK, body
	}

	// Each location is independent, so a failure in one still renders the others.
	results := make([]any, len(opts))
	var wg sync.WaitGroup
	for i, o := range opts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			g, err := BuildGlance(ctx, nws, o, now)
			if err != nil {
				log.Printf("error for %s: %v", o.Name, err)
				name := o.Name
				if name == "" {
					name = fmt.Sprintf("%.4f,%.4f", o.Lat, o.Lon)
				}
				results[i] = LocationError{Location: name, Error: err.Error()}
				return
			}
			results[i] = g
		}()
	}
	wg.Wait()
	failed := 0
	for _, r := range results {
		if _, ok := r.(LocationError); ok {
			failed++
		}
	}
	if failed == len(opts) {
		return errorResponse(fmt.Errorf("no forecasts available: %s", results[0].(LocationError).Error))
	}
	body, _ := json.Marshal(map[string]any{"locations": results, "count": len(results)})
	return http.StatusOK, body
}

func errorResponse(err error) (int, []byte) {
	status := http.StatusBadGateway
	if he, ok := err.(*httpError); ok {
		status = he.status
	}
	log.Printf("error: %v", err)
	body, _ := json.Marshal(map[string]string{"error": err.Error()})
	return status, body
}

func lambdaHandler(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	q := url.Values{}
	for k, v := range req.QueryStringParameters {
		q.Set(k, v)
	}
	status, body := serve(ctx, q)
	return events.LambdaFunctionURLResponse{
		StatusCode: status,
		Headers:    map[string]string{"Content-Type": "application/json", "Cache-Control": "max-age=300"},
		Body:       string(body),
	}, nil
}

func main() {
	if os.Getenv("AWS_LAMBDA_RUNTIME_API") != "" {
		lambda.Start(lambdaHandler)
		return
	}

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		status, body := serve(r.Context(), r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write(body)
	})
	addr := ":" + envOr("PORT", "8080")
	fmt.Printf("listening on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
