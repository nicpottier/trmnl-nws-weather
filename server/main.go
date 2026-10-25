// Command trmnl-nws-weather serves a compact NWS forecast for the TRMNL "NWS Weather Glance" plugin.
//
// It runs as an AWS Lambda behind a Function URL when AWS_LAMBDA_RUNTIME_API is set,
// and as a plain HTTP server on $PORT (default 8080) otherwise.
//
//	GET /?lat_lon=47.5962,-120.6615&units=us&name=Leavenworth,%20WA&days=7
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

func parseOptions(q url.Values) (Options, error) {
	// Optional shared secret so a public Function URL isn't an open NWS proxy.
	if token := os.Getenv("ACCESS_TOKEN"); token != "" &&
		subtle.ConstantTimeCompare([]byte(q.Get("token")), []byte(token)) != 1 {
		return Options{}, &httpError{http.StatusUnauthorized, "invalid token"}
	}

	latLon := q.Get("lat_lon")
	if latLon == "" && q.Get("lat") != "" {
		latLon = q.Get("lat") + "," + q.Get("lon")
	}
	latS, lonS, ok := strings.Cut(latLon, ",")
	lat, err1 := strconv.ParseFloat(strings.TrimSpace(latS), 64)
	lon, err2 := strconv.ParseFloat(strings.TrimSpace(lonS), 64)
	if !ok || err1 != nil || err2 != nil {
		return Options{}, &httpError{http.StatusBadRequest, "lat_lon must look like 47.5962,-120.6615"}
	}

	units := "us"
	switch strings.ToLower(q.Get("units")) {
	case "si", "c", "celsius", "metric":
		units = "si"
	}
	days, err := strconv.Atoi(q.Get("days"))
	if err != nil || days < 1 || days > 7 {
		days = 7
	}
	return Options{Lat: lat, Lon: lon, Units: units, Name: strings.TrimSpace(q.Get("name")), Days: days}, nil
}

// serve returns the status and JSON body for a request's query string.
func serve(ctx context.Context, q url.Values) (int, []byte) {
	opts, err := parseOptions(q)
	if err == nil {
		var g *Glance
		if g, err = BuildGlance(ctx, nws, opts, time.Now()); err == nil {
			body, _ := json.Marshal(g)
			return http.StatusOK, body
		}
	}
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
