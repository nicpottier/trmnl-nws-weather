// Command trmnl-nws-weather serves a compact NWS forecast for the TRMNL "NWS Forecast Chart" plugin.
//
// It runs as an AWS Lambda behind a Function URL when AWS_LAMBDA_RUNTIME_API is set,
// and as a plain HTTP server on $PORT (default 8080) otherwise. On Vercel, api/index.go
// serves the same forecast.Handler instead.
//
//	GET /?locations=Leavenworth,%20WA|47.5962,-120.6615;Stevens%20Pass|47.7448,-121.0890&units=us
//	GET /?lat_lon=47.5962,-120.6615&name=Leavenworth,%20WA
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"

	"github.com/nicpottier/trmnl-nws-weather/forecast"
)

func lambdaHandler(ctx context.Context, req events.LambdaFunctionURLRequest) (events.LambdaFunctionURLResponse, error) {
	q := url.Values{}
	for k, v := range req.QueryStringParameters {
		q.Set(k, v)
	}
	status, body := forecast.Serve(ctx, q)
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

	http.HandleFunc("/", forecast.Handler)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	fmt.Printf("listening on %s\n", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
