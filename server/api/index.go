// Package handler is the Vercel serverless function: Vercel serves its exported Handler at /api
// (and at / through the rewrite in vercel.json).
package handler

import (
	"net/http"

	"github.com/nicpottier/trmnl-nws-weather/forecast"
)

func Handler(w http.ResponseWriter, r *http.Request) {
	forecast.Handler(w, r)
}
