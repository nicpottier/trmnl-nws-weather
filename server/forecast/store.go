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
