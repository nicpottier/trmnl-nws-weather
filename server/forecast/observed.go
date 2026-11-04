package forecast

import (
	"math"
	"sort"
	"time"
)

// Measured snow and rain, from the stations near a location (via Synoptic).
//
// Snowfall isn't measured directly: the stations' "new snow" values are unusable (a steady
// 0.3″ an hour with bare ground, or 110″ an hour). Snow depth is real but noisy (±1″ hour to
// hour at Stevens Pass - Schmidt Haus) and has sensor faults (Brooks read 270″ for three days),
// and it settles between storms. So new snow is the rise in a station's cleaned-up depth: each
// station's readings are reduced to one per hour, single-hour spikes and impossible values are
// dropped, a 3-hour median smooths the rest, and a jump of more than 8″ in an hour is treated
// as a sensor fault rather than snow. A rise only counts while the station's own thermometer
// reads 35°F or colder: sunny afternoons nudge depth sensors up an inch with no snow falling.
// The largest qualifying rise among the stations wins, since snow often accumulates at the
// upper stations while the lower ones get rain, but only when a second station also rose by
// the minimum: one station's sensor alone (some report whole inches and flip between 1″ and
// 2″) isn't enough. Stations whose depth typically swings more than an inch from one hour to
// the next are left out entirely.
//
// Rain comes from precipitation gauges, but only those that recorded anything at all in the
// fetched window: a dead gauge reads a steady zero (Schmidt Haus and Grace Lakes did all week).
type observed struct {
	depths []depthSeries // per snow-depth station
	gauges [][]reading   // per live gauge: hourly precipitation, inches
}

type depthSeries struct {
	depth []reading         // smoothed hourly snow depth, inches
	tempF map[int64]float64 // the station's hourly temperature, by hour start (Unix)
}

const (
	maxPlausibleDepth = 400.0 // inches; anything deeper is a sensor fault
	maxSpike          = 4.0   // an hour this far from its neighbors' median is noise
	maxHourlyStep     = 8.0   // a smoothed jump this big in an hour is a fault, not snowfall
	barSnowMinRise    = 0.5   // per 6-hour bar: smaller rises at a station are noise
	freshSnowMinRise  = 1.0   // for the 24/48-hour totals
	maxSnowTempF      = 35.0  // warmer than this, a depth rise isn't new snow
	maxTypicalJitter  = 1.0   // inches; a station whose median hourly change is larger is too noisy
)

// newObserved gathers the snow-depth stations and live gauges near the point's elevation, or
// returns nil when there are neither.
func newObserved(series []stationSeries, pointElevM float64) *observed {
	o := &observed{}
	for i := range series {
		s := &series[i]
		if !s.nearElevation(pointElevM) {
			continue
		}
		if d := smoothDepth(s.Depth); len(d) >= 3 && jitter(s.Depth) <= maxTypicalJitter {
			o.depths = append(o.depths, depthSeries{d, s.hourly()})
		}
		total := 0.0
		for _, p := range s.Precip {
			total += math.Max(p.V, 0)
		}
		if total > 0 {
			o.gauges = append(o.gauges, s.Precip)
		}
	}
	if len(o.depths) == 0 && len(o.gauges) == 0 {
		return nil
	}
	return o
}

// snow returns the new snow, in inches, that fell between from and to: the largest rise of any
// station's depth within the window, if it's at least minRise and (with more than one station)
// another station also rose at least minRise.
func (o *observed) snow(from, to time.Time, minRise float64) float64 {
	rises := make([]float64, len(o.depths))
	for i, d := range o.depths {
		rises[i] = depthRise(d, from, to)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(rises)))
	if len(rises) == 0 || rises[0] < minRise || (len(rises) > 1 && rises[1] < minRise) {
		return 0
	}
	return rises[0]
}

// jitter is the median change between consecutive hours of a station's raw depth readings.
func jitter(raw []reading) float64 {
	byHour := map[int64][]float64{}
	for _, r := range raw {
		if r.V >= 0 && r.V <= maxPlausibleDepth {
			h := r.T.Truncate(time.Hour).Unix()
			byHour[h] = append(byHour[h], r.V)
		}
	}
	hours := make([]int64, 0, len(byHour))
	for h := range byHour {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
	steps := []float64{}
	for i := 1; i < len(hours); i++ {
		if hours[i]-hours[i-1] == 3600 {
			steps = append(steps, math.Abs(median(byHour[hours[i]])-median(byHour[hours[i-1]])))
		}
	}
	return median(steps)
}

// rain returns the precipitation, in inches, recorded in (from, to], the median of the live
// gauges.
func (o *observed) rain(from, to time.Time) float64 {
	totals := []float64{}
	for _, g := range o.gauges {
		t := 0.0
		for _, p := range g {
			if p.T.After(from) && !p.T.After(to) {
				t += math.Max(p.V, 0)
			}
		}
		totals = append(totals, t)
	}
	return median(totals)
}

// hasSnowpack reports whether any station shows at least an inch of snow on the ground now.
func (o *observed) hasSnowpack() bool {
	for _, d := range o.depths {
		if d.depth[len(d.depth)-1].V >= 1 {
			return true
		}
	}
	return false
}

// smoothDepth turns raw depth readings into a smoothed hourly series (see the type comment).
func smoothDepth(raw []reading) []reading {
	byHour := map[int64][]float64{}
	for _, r := range raw {
		if r.V >= 0 && r.V <= maxPlausibleDepth {
			h := r.T.Truncate(time.Hour).Unix()
			byHour[h] = append(byHour[h], r.V)
		}
	}
	hours := make([]int64, 0, len(byHour))
	for h := range byHour {
		hours = append(hours, h)
	}
	sort.Slice(hours, func(i, j int) bool { return hours[i] < hours[j] })
	hourly := make([]reading, len(hours))
	for i, h := range hours {
		hourly[i] = reading{time.Unix(h, 0).UTC(), median(byHour[h])}
	}

	// Drop single-hour spikes: values far from the median of up to two hours either side.
	kept := []reading{}
	for i, r := range hourly {
		nb := []float64{}
		for j := max(0, i-2); j <= min(len(hourly)-1, i+2); j++ {
			if j != i {
				nb = append(nb, hourly[j].V)
			}
		}
		if len(nb) > 0 && math.Abs(r.V-median(nb)) > maxSpike {
			continue
		}
		kept = append(kept, r)
	}

	smoothed := make([]reading, len(kept))
	for i := range kept {
		w := []float64{}
		for j := max(0, i-1); j <= min(len(kept)-1, i+1); j++ {
			w = append(w, kept[j].V)
		}
		smoothed[i] = reading{kept[i].T, median(w)}
	}
	return smoothed
}

// depthRise is the largest rise within [from, to] of a smoothed depth series: the most the depth
// climbed above its lowest point earlier in the window, counting only hours at or below
// maxSnowTempF (or with no temperature). A step of more than maxHourlyStep restarts the count,
// so a sensor jumping between levels isn't read as snowfall.
func depthRise(d depthSeries, from, to time.Time) float64 {
	best, low, prev := 0.0, math.Inf(1), math.NaN()
	for _, r := range d.depth {
		if r.T.Before(from) || r.T.After(to) {
			continue
		}
		if math.Abs(r.V-prev) > maxHourlyStep {
			low = math.Inf(1)
		}
		low = math.Min(low, r.V)
		if t, ok := d.tempF[r.T.Unix()]; !ok || t <= maxSnowTempF {
			best = math.Max(best, r.V-low)
		}
		prev = r.V
	}
	return best
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}
