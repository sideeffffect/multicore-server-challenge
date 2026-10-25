package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strings"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// The load-box CPU limit. The pass/fail thresholds depend on the track (track.go).
const lgCPULimitPct = 75

type Stats struct {
	Count        int64            `json:"count"`
	RPS          float64          `json:"rps"`
	P50Ms        float64          `json:"p50_ms"`
	P90Ms        float64          `json:"p90_ms"`
	P95Ms        float64          `json:"p95_ms"`
	P99Ms        float64          `json:"p99_ms"`
	P999Ms       float64          `json:"p999_ms"`
	MaxMs        float64          `json:"max_ms"`
	MeanMs       float64          `json:"mean_ms"`
	Errors       int64            `json:"errors"`
	ErrorRate    float64          `json:"error_rate"`
	Statuses     map[string]int64 `json:"statuses,omitempty"`
	ErrorClasses map[string]int64 `json:"error_classes,omitempty"`
	ErrorExample string           `json:"error_example,omitempty"`
}

type EndpointStats struct {
	Name string `json:"name"`
	Stats
}

type BoxSummary struct {
	File       string   `json:"file,omitempty"`
	Index      int      `json:"box_index"`
	Host       string   `json:"host"`
	Users      int      `json:"users"`
	CPUAvg     *float64 `json:"cpu_avg_measure_pct"`
	CPUMax     *float64 `json:"cpu_max_measure_pct"`
	ProcCPUAvg float64  `json:"proc_cpu_avg_measure_pct"`
	RSSPeakMB  float64  `json:"rss_peak_mb"`
	Behind     bool     `json:"fell_behind"`
	BehindWhy  []string `json:"fell_behind_reasons,omitempty"`
	Count      int64    `json:"requests_in_window"`
	Dials      int64    `json:"tcp_connects"`
}

type Verdict struct {
	Pass      bool     `json:"pass"`
	Reasons   []string `json:"fail_reasons,omitempty"`
	LGLimited bool     `json:"lg_limited"`
	LGReasons []string `json:"lg_reasons,omitempty"`
}

type Mix struct {
	PageViews     int64              `json:"page_views"`
	CallsPerPage  float64            `json:"calls_per_page"`
	ReqPerUserS   float64            `json:"req_per_user_s"`
	WritePct      float64            `json:"write_pct"`
	PagePct       map[string]float64 `json:"page_pct"`
	SessionStarts map[string]int64   `json:"session_starts"`
}

type MergedSecond struct {
	T         int      `json:"t"`
	Started   int64    `json:"started"`
	Completed int64    `json:"completed"`
	Errors    int64    `json:"errors"`
	Active    int64    `json:"active_vus"`
	P50Ms     float64  `json:"p50_ms"`
	P95Ms     float64  `json:"p95_ms"`
	P99Ms     float64  `json:"p99_ms"`
	MaxBoxCPU *float64 `json:"max_box_cpu_pct"`
}

type Merged struct {
	Track     string          `json:"track"`
	Users     int             `json:"users"`
	RunIDs    []string        `json:"run_ids"`
	Target    string          `json:"target"`
	StartAtMs int64           `json:"start_at_ms"`
	RampS     float64         `json:"ramp_s"`
	WindowS   float64         `json:"measure_s"`
	Verdict   Verdict         `json:"verdict"`
	Overall   Stats           `json:"overall"`
	Endpoints []EndpointStats `json:"endpoints"`
	Mix       Mix             `json:"mix"`
	Boxes     []BoxSummary    `json:"boxes"`
	Timeline  []MergedSecond  `json:"timeline"`
	Hist      string          `json:"hist,omitempty"`
}

func cmdMerge(args []string) int {
	fs := flag.NewFlagSet("merge", flag.ExitOnError)
	out := fs.String("o", "merged.json", "output JSON ('-' = stdout, '' = none)")
	fs.Parse(args)
	if fs.NArg() == 0 {
		fmt.Fprintln(os.Stderr, "merge: give one or more box result files")
		return 2
	}
	var rs []*BoxResult
	for _, f := range fs.Args() {
		r, err := readBoxResult(f)
		if err != nil {
			fmt.Fprintln(os.Stderr, "merge:", err)
			return 1
		}
		rs = append(rs, r)
	}
	m := mergeResults(rs)
	for i := range m.Boxes {
		m.Boxes[i].File = fs.Arg(i)
	}
	if *out != "" {
		if err := writeJSON(*out, m); err != nil {
			fmt.Fprintln(os.Stderr, "merge:", err)
			return 1
		}
	}
	printMerged(os.Stdout, m)
	if !m.Verdict.Pass {
		return 3
	}
	return 0
}

func readBoxResult(path string) (*BoxResult, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r BoxResult
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

func statsOf(h *hdr.Histogram, windowS float64) Stats {
	s := Stats{Count: h.TotalCount()}
	if s.Count == 0 {
		return s
	}
	ms := func(q float64) float64 { return float64(h.ValueAtQuantile(q)) / 1000 }
	s.P50Ms, s.P90Ms, s.P95Ms, s.P99Ms, s.P999Ms = ms(50), ms(90), ms(95), ms(99), ms(99.9)
	s.MaxMs = float64(h.Max()) / 1000
	s.MeanMs = h.Mean() / 1000
	if windowS > 0 {
		s.RPS = float64(s.Count) / windowS
	}
	return s
}

func mergeResults(rs []*BoxResult) *Merged {
	m := &Merged{}
	if len(rs) == 0 {
		return m
	}
	base := rs[0].Meta
	m.StartAtMs, m.RampS, m.WindowS, m.Target = base.StartAtMs, base.RampS, base.MeasureS, base.Target
	m.Track = base.Track
	if m.Track == "" {
		m.Track = "standard" // results from before tracks existed
	}
	overall := newHist()
	eps := map[string]*hdr.Histogram{}
	epStats := map[string]*Stats{}
	pages := map[string]int64{}
	sessions := map[string]int64{}
	type secAcc struct {
		ms   MergedSecond
		hist *hdr.Histogram
	}
	secs := map[int]*secAcc{}
	for _, r := range rs {
		m.Users += r.Meta.Users
		m.RunIDs = append(m.RunIDs, r.Meta.RunID)
		bs := BoxSummary{Index: r.Meta.BoxIndex, Host: r.Meta.Host, Users: r.Meta.Users, CPUAvg: r.Box.CPUAvg, CPUMax: r.Box.CPUMax,
			ProcCPUAvg: r.Box.ProcCPUAvg, RSSPeakMB: r.Box.RSSPeakMB, Dials: r.Box.Dials, Behind: r.Box.Behind, BehindWhy: r.Box.BehindWhy}
		for name, e := range r.Endpoints {
			h, err := decodeHist(e.Hist)
			if err != nil {
				continue
			}
			bs.Count += h.TotalCount()
			if eps[name] == nil {
				eps[name] = newHist()
				epStats[name] = &Stats{Statuses: map[string]int64{}, ErrorClasses: map[string]int64{}}
			}
			eps[name].Merge(h)
			overall.Merge(h)
			st := epStats[name]
			for k, n := range e.Statuses {
				st.Statuses[k] += n
			}
			for k, n := range e.Errors {
				st.ErrorClasses[k] += n
				st.Errors += n
			}
			if st.ErrorExample == "" {
				st.ErrorExample = e.ErrorEx
			}
		}
		m.Boxes = append(m.Boxes, bs)
		for k, n := range r.Pages {
			pages[k] += n
		}
		for k, n := range r.Sessions {
			sessions[k] += n
		}
		shift := int(math.Round(float64(r.Meta.StartAtMs-base.StartAtMs) / 1000))
		for _, s := range r.Timeline {
			t := s.T + shift
			a := secs[t]
			if a == nil {
				a = &secAcc{ms: MergedSecond{T: t}, hist: newHist()}
				secs[t] = a
			}
			a.ms.Started += s.Started
			a.ms.Completed += s.Completed
			a.ms.Errors += s.Errors
			a.ms.Active += s.Active
			if s.CPU != nil && (a.ms.MaxBoxCPU == nil || *s.CPU > *a.ms.MaxBoxCPU) {
				c := *s.CPU
				a.ms.MaxBoxCPU = &c
			}
			if h, err := decodeHist(s.Hist); err == nil {
				a.hist.Merge(h)
			}
		}
	}

	// endpoints and overall
	all := Stats{Statuses: map[string]int64{}, ErrorClasses: map[string]int64{}}
	var writes int64
	for name, h := range eps {
		st := statsOf(h, m.WindowS)
		src := epStats[name]
		st.Statuses, st.ErrorClasses, st.Errors, st.ErrorExample = src.Statuses, src.ErrorClasses, src.Errors, src.ErrorExample
		if st.Count > 0 {
			st.ErrorRate = float64(st.Errors) / float64(st.Count)
		}
		m.Endpoints = append(m.Endpoints, EndpointStats{Name: name, Stats: st})
		for k, n := range st.Statuses {
			all.Statuses[k] += n
		}
		for k, n := range st.ErrorClasses {
			all.ErrorClasses[k] += n
		}
		all.Errors += st.Errors
		if all.ErrorExample == "" {
			all.ErrorExample = st.ErrorExample
		}
		if isWriteLabel(name) {
			writes += st.Count
		}
	}
	sort.Slice(m.Endpoints, func(i, j int) bool { return m.Endpoints[i].Count > m.Endpoints[j].Count })
	o := statsOf(overall, m.WindowS)
	o.Statuses, o.ErrorClasses, o.Errors, o.ErrorExample = all.Statuses, all.ErrorClasses, all.Errors, all.ErrorExample
	if o.Count > 0 {
		o.ErrorRate = float64(o.Errors) / float64(o.Count)
	}
	m.Overall = o
	m.Hist = encodeHist(overall)

	// mix
	var pv int64
	for _, n := range pages {
		pv += n
	}
	m.Mix = Mix{PageViews: pv, PagePct: map[string]float64{}, SessionStarts: sessions}
	if pv > 0 {
		m.Mix.CallsPerPage = float64(o.Count) / float64(pv)
		for k, n := range pages {
			m.Mix.PagePct[k] = 100 * float64(n) / float64(pv)
		}
	}
	if o.Count > 0 {
		m.Mix.WritePct = 100 * float64(writes) / float64(o.Count)
	}
	if m.Users > 0 {
		m.Mix.ReqPerUserS = o.RPS / float64(m.Users)
	}

	// timeline
	for _, a := range secs {
		if a.hist.TotalCount() > 0 {
			a.ms.P50Ms = float64(a.hist.ValueAtQuantile(50)) / 1000
			a.ms.P95Ms = float64(a.hist.ValueAtQuantile(95)) / 1000
			a.ms.P99Ms = float64(a.hist.ValueAtQuantile(99)) / 1000
		}
		m.Timeline = append(m.Timeline, a.ms)
	}
	sort.Slice(m.Timeline, func(i, j int) bool { return m.Timeline[i].T < m.Timeline[j].T })

	// verdict
	v := &m.Verdict
	if o.Count == 0 {
		v.Reasons = append(v.Reasons, "no requests in the measure window")
	}
	tr, err := trackNamed(m.Track)
	if err != nil {
		v.Reasons = append(v.Reasons, err.Error())
	}
	for _, r := range rs[1:] {
		if r.Meta.Track != rs[0].Meta.Track {
			v.Reasons = append(v.Reasons, fmt.Sprintf("boxes ran different tracks (%q, %q)", rs[0].Meta.Track, r.Meta.Track))
			break
		}
	}
	v.Reasons = append(v.Reasons, tr.failures(o)...)
	v.Pass = len(v.Reasons) == 0
	for _, b := range m.Boxes {
		if b.CPUAvg != nil && *b.CPUAvg > lgCPULimitPct {
			v.LGReasons = append(v.LGReasons, fmt.Sprintf("box %d CPU %.0f%% > %d%%", b.Index, *b.CPUAvg, lgCPULimitPct))
		}
		if b.Behind {
			v.LGReasons = append(v.LGReasons, fmt.Sprintf("box %d fell behind: %s", b.Index, strings.Join(b.BehindWhy, "; ")))
		}
	}
	v.LGLimited = len(v.LGReasons) > 0
	return m
}

func fmtPct(p *float64) string {
	if p == nil {
		return "N/A"
	}
	return fmt.Sprintf("%.0f%%", *p)
}

func printMerged(w io.Writer, m *Merged) {
	fmt.Fprintf(w, "\n%d users on %d box(es), %s, measured %.0f s after a %.0f s ramp\n", m.Users, len(m.Boxes), m.Target, m.WindowS, m.RampS)
	fmt.Fprintf(w, "%-42s %8s %8s %8s %8s %8s %8s %8s %7s\n", "endpoint", "count", "req/s", "p50", "p90", "p95", "p99", "max", "err%")
	row := func(name string, s Stats) {
		fmt.Fprintf(w, "%-42s %8d %8.1f %8.1f %8.1f %8.1f %8.1f %8.0f %7.2f\n", name, s.Count, s.RPS, s.P50Ms, s.P90Ms, s.P95Ms, s.P99Ms, s.MaxMs, 100*s.ErrorRate)
	}
	for _, e := range m.Endpoints {
		row(e.Name, e.Stats)
	}
	row("ALL", m.Overall)
	if len(m.Overall.ErrorClasses) > 0 {
		var parts []string
		for k, n := range m.Overall.ErrorClasses {
			parts = append(parts, fmt.Sprintf("%s=%d", k, n))
		}
		sort.Strings(parts)
		fmt.Fprintf(w, "errors: %s  (e.g. %q)\n", strings.Join(parts, " "), m.Overall.ErrorExample)
	}
	fmt.Fprintf(w, "mix: %.2f calls/page, %.3f req/s per user, %.1f%% writes, %d page views\n", m.Mix.CallsPerPage, m.Mix.ReqPerUserS, m.Mix.WritePct, m.Mix.PageViews)
	for _, b := range m.Boxes {
		fmt.Fprintf(w, "box %d (%s): %d users, CPU avg %s max %s, loadgen proc %.0f%%, RSS peak %.0f MB, %d TCP connects, behind=%v\n",
			b.Index, b.Host, b.Users, fmtPct(b.CPUAvg), fmtPct(b.CPUMax), b.ProcCPUAvg, b.RSSPeakMB, b.Dials, b.Behind)
	}
	fmt.Fprintf(w, "tail: p99.9 %.1f ms, max %.0f ms\n", m.Overall.P999Ms, m.Overall.MaxMs)
	verdict := "PASS"
	if !m.Verdict.Pass {
		verdict = "FAIL (" + strings.Join(m.Verdict.Reasons, ", ") + ")"
	}
	criteria := "unknown track"
	if tr, err := trackNamed(m.Track); err == nil {
		criteria = tr.name + " track: " + tr.criteria()
	}
	fmt.Fprintf(w, "verdict: %s   [%s]\n", verdict, criteria)
	if m.Verdict.LGLimited {
		fmt.Fprintf(w, "LG-LIMITED: %s\n", strings.Join(m.Verdict.LGReasons, "; "))
	} else {
		fmt.Fprintln(w, "lg_limited: no")
	}
}

// oneLine is the search log's summary of a merged run.
func oneLine(m *Merged) string {
	v := "PASS"
	if !m.Verdict.Pass {
		v = "FAIL"
	}
	if m.Verdict.LGLimited {
		v += " LG-LIMITED"
	}
	cpus := []string{}
	for _, b := range m.Boxes {
		cpus = append(cpus, fmtPct(b.CPUAvg))
	}
	return fmt.Sprintf("%6d users  %7.0f req/s  p50 %6.1f  p95 %7.1f  p99 %7.1f ms  err %5.2f%%  lgCPU %s  %s",
		m.Users, m.Overall.RPS, m.Overall.P50Ms, m.Overall.P95Ms, m.Overall.P99Ms, 100*m.Overall.ErrorRate, strings.Join(cpus, "/"), v)
}
