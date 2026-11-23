package forecast

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// timings records how long each of a forecast's sources took, for the logs.
type timings struct {
	start time.Time
	mu    sync.Mutex
	took  map[string]time.Duration
}

func newTimings() *timings { return &timings{start: time.Now(), took: map[string]time.Duration{}} }

func (t *timings) add(name string, d time.Duration) {
	t.mu.Lock()
	t.took[name] = d
	t.mu.Unlock()
}

// run runs f, records its duration under name and marks wg done.
func (t *timings) run(wg *sync.WaitGroup, name string, f func()) {
	defer wg.Done()
	start := time.Now()
	f()
	t.add(name, time.Since(start))
}

// String lists the sources slowest first, then the total so far.
func (t *timings) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	names := make([]string, 0, len(t.took))
	for n := range t.took {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return t.took[names[i]] > t.took[names[j]] })
	parts := []string{}
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s %.2fs", n, t.took[n].Seconds()))
	}
	return strings.Join(parts, ", ") + fmt.Sprintf(" · total %.2fs", time.Since(t.start).Seconds())
}

// A whole refresh gets requestBudget; the optional sources (alerts, stations, SNOTEL, home-station
// rain, the fallback current reading) must be back within optionalBudget of its start, or the
// screen is drawn without them. Each request gives up after requestTimeout, and is retried (once,
// on a server error or a dropped connection) only with at least minRetryTime left.
var (
	requestBudget  = 10 * time.Second
	optionalBudget = 7 * time.Second
	requestTimeout = 5 * time.Second
	minRetryTime   = 2 * time.Second
)

// timeLeft is how long until ctx's deadline (a long time when it has none).
func timeLeft(ctx context.Context) time.Duration {
	if d, ok := ctx.Deadline(); ok {
		return time.Until(d)
	}
	return time.Hour
}
