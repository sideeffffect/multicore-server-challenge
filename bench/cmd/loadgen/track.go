package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A track is a way to score the challenge: how fast the virtual users go, and what a passing run must meet.
//
//   - standard: the challenge as scored on the leaderboard (README.md, "Scoring").
//   - tail: the tail-latency track (README.md, "Tail-latency track"): users five times slower, so many more of them
//     hold connections for the same request rate, and a run passes only if the slowest requests stay fast too
//     (p99.9 and p99.99), over longer windows. A stop-the-world pause delays every request in flight at once, so one
//     pause longer than the p99.99 limit plus 0.01% of the window (60 ms of a 10-minute hold) fails it, while a few
//     unrelated outliers (a retransmitted SYN, a network hiccup) don't.
//
// A threshold of 0 is not checked.
type track struct {
	name         string
	thinkScale   float64 // think times and session lengths, multiplied
	maxP95Ms     float64
	maxP99Ms     float64
	maxP999Ms    float64
	maxP9999Ms   float64
	maxErrRate   float64
	probeMeasure time.Duration // search defaults
	holdMeasure  time.Duration
}

var tracks = map[string]track{
	"standard": {name: "standard", thinkScale: 1, maxP95Ms: 500, maxP99Ms: 1000, maxErrRate: 0.01,
		probeMeasure: 60 * time.Second, holdMeasure: 300 * time.Second},
	"tail": {name: "tail", thinkScale: 5, maxP99Ms: 250, maxP999Ms: 500, maxP9999Ms: 1000, maxErrRate: 0.001,
		probeMeasure: 180 * time.Second, holdMeasure: 600 * time.Second},
}

func trackNamed(name string) (track, error) {
	if name == "" {
		name = "standard"
	}
	t, ok := tracks[name]
	if !ok {
		var names []string
		for n := range tracks {
			names = append(names, n)
		}
		sort.Strings(names)
		return track{}, fmt.Errorf("unknown track %q (one of: %s)", name, strings.Join(names, ", "))
	}
	return t, nil
}

// failures lists every threshold the overall stats miss.
func (t track) failures(o Stats) []string {
	var r []string
	check := func(label string, v, limit float64) {
		if limit > 0 && v >= limit {
			r = append(r, fmt.Sprintf("%s %.0f ms >= %.0f", label, v, limit))
		}
	}
	check("p95", o.P95Ms, t.maxP95Ms)
	check("p99", o.P99Ms, t.maxP99Ms)
	check("p99.9", o.P999Ms, t.maxP999Ms)
	check("p99.99", o.P9999Ms, t.maxP9999Ms)
	if t.maxErrRate > 0 && o.ErrorRate >= t.maxErrRate {
		r = append(r, fmt.Sprintf("errors %.2f%% >= %.1f%%", 100*o.ErrorRate, 100*t.maxErrRate))
	}
	return r
}

// criteria describes the pass criteria, e.g. "p95 < 500 ms, p99 < 1000 ms, errors < 1%".
func (t track) criteria() string {
	var c []string
	add := func(label string, limit float64) {
		if limit > 0 {
			c = append(c, fmt.Sprintf("%s < %.0f ms", label, limit))
		}
	}
	add("p95", t.maxP95Ms)
	add("p99", t.maxP99Ms)
	add("p99.9", t.maxP999Ms)
	add("p99.99", t.maxP9999Ms)
	c = append(c, fmt.Sprintf("errors < %s%%", strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.1f", 100*t.maxErrRate), "0"), ".")))
	return strings.Join(c, ", ")
}
