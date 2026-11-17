package forecast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

// stateStore keeps state between runs: home-station rain reports (aprs.go) and SNOTEL readings and
// station lists (snotel.go), so the service can bridge gaps and outages. Nil when not configured.
var stateStore = newRedisStore()

// redisStore keeps a little state between runs in Upstash Redis, through its REST API. Vercel's
// Upstash integration sets KV_REST_API_URL and KV_REST_API_TOKEN; Upstash itself names them
// UPSTASH_REDIS_REST_URL and UPSTASH_REDIS_REST_TOKEN.
type redisStore struct {
	url, token string
	http       *http.Client
}

func newRedisStore() *redisStore {
	url, token := os.Getenv("KV_REST_API_URL"), os.Getenv("KV_REST_API_TOKEN")
	if url == "" {
		url, token = os.Getenv("UPSTASH_REDIS_REST_URL"), os.Getenv("UPSTASH_REDIS_REST_TOKEN")
	}
	if url == "" || token == "" {
		return nil
	}
	return &redisStore{url, token, &http.Client{Timeout: 5 * time.Second}}
}

// pipeline runs Redis commands in one request and returns each one's result.
func (r *redisStore) pipeline(ctx context.Context, cmds ...[]any) ([]json.RawMessage, error) {
	body, err := json.Marshal(cmds)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.url+"/pipeline", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("redis: %s", resp.Status)
	}
	var out []struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	results := make([]json.RawMessage, len(out))
	for i, o := range out {
		if o.Error != "" {
			return nil, fmt.Errorf("redis: %s", o.Error)
		}
		results[i] = o.Result
	}
	return results, nil
}

// saveJSON stores a value under key for ttl, best effort.
func saveJSON(ctx context.Context, key string, v any, ttl time.Duration) {
	if stateStore == nil {
		return
	}
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	stateStore.pipeline(ctx, []any{"SET", key, string(b), "EX", int(ttl.Seconds())})
}

// loadJSON reads a value saved with saveJSON.
func loadJSON[T any](ctx context.Context, key string) (T, bool) {
	var v T
	if stateStore == nil {
		return v, false
	}
	res, err := stateStore.pipeline(ctx, []any{"GET", key})
	if err != nil || len(res) != 1 {
		return v, false
	}
	var s *string
	if json.Unmarshal(res[0], &s) != nil || s == nil || json.Unmarshal([]byte(*s), &v) != nil {
		return v, false
	}
	return v, true
}
