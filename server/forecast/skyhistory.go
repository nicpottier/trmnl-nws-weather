package forecast

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// NWS forecasts start when they're issued, so the chart's past has no sky cover or fog, and no
// station reports clouds. Each run saves the latest forecast for the current hour (and the next, in
// case a run is missed), overwriting it until the hour has passed; what's left for each past hour
// is the forecast made as it arrived. addSkyHistory saves those and gives the grid the saved hours
// it no longer covers.

const skyHistoryKeep = 72 * time.Hour

type skyHistoryHour struct {
	Sky float64 `json:"s"`
	Fog float64 `json:"f"`
}

func addSkyHistory(ctx context.Context, grid *GridData, lat, lon float64, now time.Time) {
	if stateStore == nil || grid == nil {
		return
	}
	key := fmt.Sprintf("sky:%.4f,%.4f", lat, lon)
	skyIvs := grid.Properties.SkyCover.intervals()
	fogs := grid.Properties.Weather.fog()
	skyAt := func(t time.Time) (float64, bool) {
		for _, iv := range skyIvs {
			if !t.Before(iv.start) && t.Before(iv.end) {
				return iv.value, true
			}
		}
		return 0, false
	}

	save := []any{"HSET", key}
	for h := now.Truncate(time.Hour); h.Before(now.Add(2 * time.Hour)); h = h.Add(time.Hour) {
		if v, ok := skyAt(h.Add(30 * time.Minute)); ok {
			b, _ := json.Marshal(skyHistoryHour{v, fogs.at(h.Add(30 * time.Minute))})
			save = append(save, strconv.FormatInt(h.Unix(), 10), string(b))
		}
	}
	cmds := [][]any{}
	if len(save) > 2 {
		cmds = append(cmds, save)
	}
	cmds = append(cmds, []any{"HGETALL", key})
	res, err := stateStore.pipeline(ctx, cmds...)
	if err != nil {
		return
	}
	var flat []string
	if json.Unmarshal(res[len(res)-1], &flat) != nil {
		return
	}

	// The grid's own first hour: saved hours before it fill the gap.
	firstForecast := now
	for _, iv := range skyIvs {
		if iv.start.Before(firstForecast) {
			firstForecast = iv.start
		}
	}
	stale := []any{"HDEL", key}
	for i := 0; i+1 < len(flat); i += 2 {
		unix, err := strconv.ParseInt(flat[i], 10, 64)
		if err != nil {
			continue
		}
		h := time.Unix(unix, 0)
		if h.Before(now.Add(-skyHistoryKeep)) {
			stale = append(stale, flat[i])
			continue
		}
		if !h.Before(firstForecast) {
			continue
		}
		r := skyHistoryHour{}
		if json.Unmarshal([]byte(flat[i+1]), &r) != nil {
			continue
		}
		v := r.Sky
		grid.Properties.SkyCover.Values = append(grid.Properties.SkyCover.Values, struct {
			ValidTime string   `json:"validTime"`
			Value     *float64 `json:"value"`
		}{h.UTC().Format(time.RFC3339) + "/PT1H", &v})
		if r.Fog > 0 {
			grid.pastFog = append(grid.pastFog, interval{h, h.Add(time.Hour), r.Fog})
		}
	}
	if len(stale) > 2 {
		stateStore.pipeline(ctx, stale)
	}
}
