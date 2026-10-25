package main

import (
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

// Endpoint labels. The list endpoint is split by filter because the four filters cost very different amounts.
const (
	LUserGet = iota
	LLogin
	LSignup
	LUserPut
	LTags
	LList
	LListTag
	LListAuthor
	LListFav
	LFeed
	LArticle
	LComments
	LProfile
	LFavPost
	LFavDel
	LCommentPost
	LCommentDel
	LFollowPost
	LFollowDel
	LArticlePost
	LArticlePut
	nLabels
)

var labelNames = [nLabels]string{
	LUserGet:     "GET /api/user",
	LLogin:       "POST /api/users/login",
	LSignup:      "POST /api/users",
	LUserPut:     "PUT /api/user",
	LTags:        "GET /api/tags",
	LList:        "GET /api/articles",
	LListTag:     "GET /api/articles?tag",
	LListAuthor:  "GET /api/articles?author",
	LListFav:     "GET /api/articles?favorited",
	LFeed:        "GET /api/articles/feed",
	LArticle:     "GET /api/articles/:slug",
	LComments:    "GET /api/articles/:slug/comments",
	LProfile:     "GET /api/profiles/:username",
	LFavPost:     "POST /api/articles/:slug/favorite",
	LFavDel:      "DELETE /api/articles/:slug/favorite",
	LCommentPost: "POST /api/articles/:slug/comments",
	LCommentDel:  "DELETE /api/articles/:slug/comments/:id",
	LFollowPost:  "POST /api/profiles/:username/follow",
	LFollowDel:   "DELETE /api/profiles/:username/follow",
	LArticlePost: "POST /api/articles",
	LArticlePut:  "PUT /api/articles/:slug",
}

// isWrite: requests that change data (login is a POST but only reads; sign-up writes).
func isWriteLabel(name string) bool {
	switch name {
	case labelNames[LSignup], labelNames[LUserPut], labelNames[LFavPost], labelNames[LFavDel], labelNames[LCommentPost],
		labelNames[LCommentDel], labelNames[LFollowPost], labelNames[LFollowDel], labelNames[LArticlePost], labelNames[LArticlePut]:
		return true
	}
	return false
}

// Page types (DESIGN.md table), for the mix report.
const (
	PHome = iota
	PYourFeed
	PTag
	PArticle
	PProfile
	PProfileFav
	PFavorite
	PComment
	PFollow
	PPublish
	PEdit
	PDeleteComment
	PSettings
	nPages
)

var pageNames = [nPages]string{"home", "your_feed", "tag", "article", "profile", "profile_favorited", "favorite_toggle",
	"comment", "follow_toggle", "publish", "edit_own_article", "delete_own_comment", "settings"}

// DESIGN.md page weights, in percent.
var pageWeights = [nPages]float64{22, 15, 8, 35.3, 8, 2, 4, 2.5, 1.5, 0.8, 0.3, 0.3, 0.3}

const (
	SReturning = iota
	SLogin
	SSignup
	nSessionKinds
)

var sessionNames = [nSessionKinds]string{"returning", "login", "signup"}

// Histogram range: 1 µs .. 60 s, 3 significant figures (values in microseconds).
const (
	histMin = 1
	histMax = 60_000_000
	histSig = 3
)

func newHist() *hdr.Histogram { return hdr.New(histMin, histMax, histSig) }

func encodeHist(h *hdr.Histogram) string {
	if h.TotalCount() == 0 {
		return ""
	}
	b, err := h.Encode(hdr.V2CompressedEncodingCookieBase)
	if err != nil {
		return ""
	}
	return string(b)
}

func decodeHist(s string) (*hdr.Histogram, error) {
	if s == "" {
		return newHist(), nil
	}
	return hdr.Decode([]byte(s))
}

func recordUs(h *hdr.Histogram, d time.Duration) {
	us := d.Microseconds()
	if us < histMin {
		us = histMin
	}
	if us > histMax {
		us = histMax
	}
	h.RecordValue(us)
}

// errClass maps a transport error to a stable class name.
func errClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, errTimeout), errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "conn_refused"
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "conn_reset"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "conn_closed"
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return "no_local_port"
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return "too_many_files"
	}
	return "other"
}

var errTimeout = errors.New("request timeout")

// ---------- recorder ----------

type epStats struct {
	hist     *hdr.Histogram
	statuses map[int]int64
	errors   map[string]int64
	errEx    string // one example error message
}

type secCounters struct {
	started, completed, errors atomic.Int64
	lagSumUs, lagN, lagMaxUs   atomic.Int64
}

// Recorder collects everything a box reports. One mutex guards the histograms (a few µs per request at
// ~15k req/s is negligible); per-second counters are atomics.
type Recorder struct {
	t0        time.Time // start-at
	winStart  time.Time
	winEnd    time.Time
	mu        sync.Mutex
	ep        [nLabels]epStats
	secHist   *hdr.Histogram // requests completed in the current second (rotated by the sampler)
	secs      []secCounters  // indexed by second since t0
	pages     [nPages]atomic.Int64
	sessions  [nSessionKinds]atomic.Int64
	active    atomic.Int64
	signupSeq atomic.Int64
	dials     atomic.Int64 // TCP connects (first connections + reconnects)
}

func newRecorder(t0 time.Time, ramp, measure, total time.Duration) *Recorder {
	r := &Recorder{t0: t0, winStart: t0.Add(ramp), winEnd: t0.Add(ramp + measure), secHist: newHist()}
	r.secs = make([]secCounters, int(total/time.Second)+120)
	for i := range r.ep {
		r.ep[i] = epStats{hist: newHist(), statuses: map[int]int64{}, errors: map[string]int64{}}
	}
	return r
}

func (r *Recorder) sec(t time.Time) *secCounters {
	i := int(t.Sub(r.t0) / time.Second)
	if i < 0 {
		i = 0
	}
	if i >= len(r.secs) {
		i = len(r.secs) - 1
	}
	return &r.secs[i]
}

func (r *Recorder) inWindow(t time.Time) bool { return !t.Before(r.winStart) && t.Before(r.winEnd) }

func (r *Recorder) started(t time.Time) { r.sec(t).started.Add(1) }

// done records one request. status is 0 when err != nil.
func (r *Recorder) done(label int, start, end time.Time, status int, err error) {
	failed := err != nil || status < 200 || status >= 400
	s := r.sec(end)
	s.completed.Add(1)
	if failed {
		s.errors.Add(1)
	}
	d := end.Sub(start)
	in := r.inWindow(start)
	r.mu.Lock()
	recordUs(r.secHist, d)
	if in {
		e := &r.ep[label]
		recordUs(e.hist, d)
		if err != nil {
			c := errClass(err)
			e.errors[c]++
			if e.errEx == "" {
				e.errEx = err.Error()
			}
		} else {
			e.statuses[status]++
			if failed {
				e.errors["http_"+strconv.Itoa(status)]++
			}
		}
	}
	r.mu.Unlock()
}

func (r *Recorder) wakeLag(now time.Time, lag time.Duration) {
	s := r.sec(now)
	us := lag.Microseconds()
	if us < 0 {
		us = 0
	}
	s.lagSumUs.Add(us)
	s.lagN.Add(1)
	for {
		m := s.lagMaxUs.Load()
		if us <= m || s.lagMaxUs.CompareAndSwap(m, us) {
			break
		}
	}
}

func (r *Recorder) page(p int, t time.Time) {
	if r.inWindow(t) {
		r.pages[p].Add(1)
	}
}

func (r *Recorder) session(k int, t time.Time) {
	if r.inWindow(t) {
		r.sessions[k].Add(1)
	}
}

// rotateSecond returns the histogram of the second that just ended and starts a new one.
func (r *Recorder) rotateSecond(spare *hdr.Histogram) *hdr.Histogram {
	spare.Reset()
	r.mu.Lock()
	h := r.secHist
	r.secHist = spare
	r.mu.Unlock()
	return h
}

// ---------- result JSON (shared by run, merge and search) ----------

type RunMeta struct {
	Version      string  `json:"version"`
	RunID        string  `json:"run_id"`
	Target       string  `json:"target"`
	Host         string  `json:"host"`
	BoxIndex     int     `json:"box_index"`
	Users        int     `json:"users"`
	IDOffset     int     `json:"id_offset"`
	StartAtMs    int64   `json:"start_at_ms"`
	RampS        float64 `json:"ramp_s"`
	MeasureS     float64 `json:"measure_s"`
	TailS        float64 `json:"tail_s"`
	Track        string  `json:"track"`
	ThinkScale   float64 `json:"think_scale"`
	Articles     int     `json:"articles"`
	NCPU         int     `json:"ncpu"`
	GOMAXPROCS   int     `json:"gomaxprocs"`
	WindowStart  int64   `json:"window_start_ms"`
	WindowEnd    int64   `json:"window_end_ms"`
	FinishedAtMs int64   `json:"finished_at_ms"`
}

type EndpointResult struct {
	Count    int64            `json:"count"`
	Statuses map[string]int64 `json:"statuses,omitempty"`
	Errors   map[string]int64 `json:"errors,omitempty"`
	ErrorEx  string           `json:"error_example,omitempty"`
	Hist     string           `json:"hist,omitempty"` // HdrHistogram V2 compressed, base64; values in µs
}

type Second struct {
	T          int      `json:"t"` // seconds since start_at
	Started    int64    `json:"started"`
	Completed  int64    `json:"completed"`
	Errors     int64    `json:"errors"`
	Active     int64    `json:"active_vus"`
	P50Ms      float64  `json:"p50_ms"`
	P95Ms      float64  `json:"p95_ms"`
	P99Ms      float64  `json:"p99_ms"`
	Hist       string   `json:"hist,omitempty"`
	LagMeanMs  float64  `json:"wake_lag_mean_ms"`
	LagMaxMs   float64  `json:"wake_lag_max_ms"`
	TickLagMs  float64  `json:"tick_lag_ms"`
	CPU        *float64 `json:"cpu_pct"`      // whole box, /proc/stat (null where unavailable)
	ProcCPU    float64  `json:"proc_cpu_pct"` // this process, % of all cores
	RSSMB      float64  `json:"rss_mb"`
	Goroutines int      `json:"goroutines"`
}

type BoxStats struct {
	CPUAvg      *float64 `json:"cpu_avg_measure_pct"`
	CPUMax      *float64 `json:"cpu_max_measure_pct"`
	ProcCPUAvg  float64  `json:"proc_cpu_avg_measure_pct"`
	RSSPeakMB   float64  `json:"rss_peak_mb"`
	Behind      bool     `json:"fell_behind"`
	BehindWhy   []string `json:"fell_behind_reasons,omitempty"`
	ActiveAtWin int64    `json:"active_vus_at_window_start"`
	Dials       int64    `json:"tcp_connects"`
	Note        string   `json:"note,omitempty"`
}

type BoxResult struct {
	Meta      RunMeta                    `json:"meta"`
	Endpoints map[string]*EndpointResult `json:"endpoints"`
	Pages     map[string]int64           `json:"pages"`
	Sessions  map[string]int64           `json:"session_starts"`
	Box       BoxStats                   `json:"box"`
	Timeline  []Second                   `json:"timeline"`
}
