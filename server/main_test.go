package main

import (
	"context"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestLambdaHandler(t *testing.T) {
	// A bad request is answered without calling NWS, so this needs no network.
	resp, err := lambdaHandler(context.Background(), events.LambdaFunctionURLRequest{
		QueryStringParameters: map[string]string{"lat_lon": "nowhere"},
	})
	if err != nil || resp.StatusCode != 400 || resp.Headers["Content-Type"] != "application/json" {
		t.Errorf("resp = %+v, %v", resp, err)
	}
}
