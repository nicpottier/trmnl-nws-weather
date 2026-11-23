package forecast

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A slow optional source (here the state's station list, the alerts and the forecast page's
// current reading) is left out once the optional budget runs out, rather than holding up the screen.
func TestSlowOptionalSourcesAreDropped(t *testing.T) {
	srv := fakeNWS(t, 0, true)
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "" || r.URL.Path == "/alerts/active" || r.URL.Path == "/MapClick.php" {
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
				return
			}
		}
		resp, err := http.Get(srv.URL + r.URL.RequestURI())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	t.Cleanup(slow.Close)

	oldNWS, oldForecast, oldSnotel := nwsBase, forecastBase, snotelBase
	oldOpt, oldReq := optionalBudget, requestBudget
	nwsBase, forecastBase, snotelBase = slow.URL, slow.URL, srv.URL+"/snotel"
	optionalBudget, requestBudget = 400*time.Millisecond, 3*time.Second
	t.Cleanup(func() {
		nwsBase, forecastBase, snotelBase = oldNWS, oldForecast, oldSnotel
		optionalBudget, requestBudget = oldOpt, oldReq
	})

	start := time.Now()
	now := time.Date(2026, 10, 25, 14, 45, 0, 0, time.UTC)
	g, err := BuildGlance(context.Background(), newNWSClient(), Options{Lat: 47.5962, Lon: -120.6615, Units: "us", Days: 7}, now)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v, want about the optional budget", took)
	}
	if !slices.Contains(g.Issues, "stations") || len(g.Alerts) != 0 || g.Chart == nil || len(g.Days) == 0 {
		t.Errorf("issues = %v, alerts = %v, chart? %v, days = %d", g.Issues, g.Alerts, g.Chart != nil, len(g.Days))
	}
}

// Server errors and dropped connections get one retry; a 4xx, which won't change, gets none.
func TestRetries(t *testing.T) {
	for _, tc := range []struct {
		status, tries int
	}{{http.StatusNotFound, 1}, {http.StatusInternalServerError, 2}, {http.StatusOK, 1}} {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			n.Add(1)
			w.WriteHeader(tc.status)
			w.Write([]byte(`{}`))
		}))
		var v map[string]any
		err := newNWSClient().get(context.Background(), srv.URL, &v)
		srv.Close()
		if int(n.Load()) != tc.tries || (err == nil) != (tc.status == http.StatusOK) {
			t.Errorf("%d: %d tries, err %v", tc.status, n.Load(), err)
		}
	}

	// No retry when the deadline is too close for one.
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), minRetryTime/2)
	defer cancel()
	var v map[string]any
	if err := newNWSClient().get(ctx, srv.URL, &v); err == nil || !strings.Contains(err.Error(), "500") || n.Load() != 1 {
		t.Errorf("near the deadline: %d tries, err %v", n.Load(), err)
	}
}
