package forecast

import (
	"fmt"
	"hash/fnv"
	"strings"
)

// areaPath turns per-block bar heights (0-100) into a smooth filled shape for an SVG clipPath
// with clipPathUnits="objectBoundingBox": x and y run 0-1 across the plot, y down from the top.
// The curve passes through each block's height at the block's center and is held flat to the
// edges; a monotone cubic keeps it from overshooting, so it never dips below the baseline
// between wet blocks or rises above the tallest one. Returns "" when every height is 0.
func areaPath(heights []float64) string { return shapePath(heights, false) }

// hangingPath is areaPath upside down: the shape hangs from the top edge.
func hangingPath(heights []float64) string { return shapePath(heights, true) }

func shapePath(heights []float64, fromTop bool) string {
	n := len(heights)
	any := false
	for _, h := range heights {
		any = any || h > 0
	}
	if n == 0 || !any {
		return ""
	}
	xs := []float64{0}
	ys := []float64{heights[0]}
	for k, h := range heights {
		xs = append(xs, (float64(k)+0.5)/float64(n))
		ys = append(ys, h)
	}
	xs, ys = append(xs, 1), append(ys, heights[n-1])
	for i := range ys {
		if fromTop {
			ys[i] = ys[i] / 100
		} else {
			ys[i] = 1 - ys[i]/100
		}
	}

	// Fritsch-Carlson tangents.
	m := len(xs)
	d := make([]float64, m-1)
	for i := 0; i < m-1; i++ {
		d[i] = (ys[i+1] - ys[i]) / (xs[i+1] - xs[i])
	}
	t := make([]float64, m)
	t[0], t[m-1] = d[0], d[m-2]
	for i := 1; i < m-1; i++ {
		if d[i-1]*d[i] <= 0 {
			t[i] = 0
		} else {
			t[i] = 3 * (xs[i+1] - xs[i-1]) / ((2*xs[i+1]-xs[i]-xs[i-1])/d[i-1] + (xs[i+1]-2*xs[i]+xs[i-1])/d[i])
		}
	}

	f := func(v float64) string { return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.4f", v), "0"), ".") }
	var b strings.Builder
	base := "1"
	if fromTop {
		base = "0"
	}
	fmt.Fprintf(&b, "M0,%sL0,%s", base, f(ys[0]))
	for i := 0; i < m-1; i++ {
		h := xs[i+1] - xs[i]
		fmt.Fprintf(&b, "C%s,%s %s,%s %s,%s", f(xs[i]+h/3), f(ys[i]+t[i]*h/3),
			f(xs[i+1]-h/3), f(ys[i+1]-t[i+1]*h/3), f(xs[i+1]), f(ys[i+1]))
	}
	fmt.Fprintf(&b, "L1,%sZ", base)
	return b.String()
}

// areaID is a short id for one location's clip paths, unique among the locations on a screen.
func areaID(lat, lon float64) string {
	h := fnv.New32a()
	fmt.Fprintf(h, "%.4f,%.4f", lat, lon)
	return fmt.Sprintf("wx%08x", h.Sum32())
}
