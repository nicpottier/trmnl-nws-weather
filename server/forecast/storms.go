package forecast

import "math"

// Storm is one label under the chart, for a run of wet (or windy) 6-hour blocks: its totals,
// e.g. ["4.0″", "0.30″"] for snow then rain, centered at X (0-100 % across the plot).
type Storm struct {
	X      float64  `json:"x"`
	Labels []string `json:"labels"`
}

// A block counts toward a storm when it gets at least this much. Lighter blocks between two wet
// ones split them into separate storms, so a week of drizzle with two real storms reads as two.
const (
	stormMinRainIn, stormMinSnowIn = 0.02, 0.2
	stormMinRainMm, stormMinSnowCm = 0.5, 0.5
)

// stormMaxLull is how many dry blocks in a row can sit inside one storm; minStormLabelGap is how
// close (in % of the chart's width) two storms' labels can be before the storms are combined. A
// label is about 7% wide on a TRMNL X.
const (
	stormMaxLull     = 1
	minStormLabelGap = 10.0
)

// joinRuns merges runs separated by at most maxGap indexes.
func joinRuns(runs [][2]int, maxGap int) [][2]int {
	out := [][2]int{}
	for _, r := range runs {
		if n := len(out); n > 0 && r[0]-out[n-1][1]-1 <= maxGap {
			out[n-1][1] = r[1]
			continue
		}
		out = append(out, r)
	}
	return out
}

// runs returns the [first, last] index of each run of consecutive blocks for which in is true.
func runs(n int, in func(int) bool) [][2]int {
	out := [][2]int{}
	for k := 0; k < n; k++ {
		if !in(k) {
			continue
		}
		start := k
		for k+1 < n && in(k+1) {
			k++
		}
		out = append(out, [2]int{start, k})
	}
	return out
}

// runCenter is the middle of a run as a percentage across n blocks, kept far enough from the
// edges that a centered label fits.
func runCenter(r [2]int, n int) float64 {
	x := float64(r[0]+r[1]+1) / 2 / float64(n) * 100
	return math.Round(math.Min(96, math.Max(4, x))*10) / 10
}
