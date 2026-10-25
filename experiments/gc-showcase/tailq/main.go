// tailq prints the tail of a `loadgen merge` result: the percentiles beyond p99 that the merge summary leaves out,
// and the share of requests slower than fixed thresholds.
//
//	go run ./tailq merged.json
package main

import (
	"encoding/json"
	"fmt"
	"os"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

func main() {
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	var merged struct {
		Hist string `json:"hist"`
	}
	if err := json.Unmarshal(raw, &merged); err != nil {
		panic(err)
	}
	h, err := hdr.Decode([]byte(merged.Hist)) // microseconds
	if err != nil {
		panic(err)
	}
	ms := func(q float64) float64 { return float64(h.ValueAtQuantile(q)) / 1000 }
	over := func(limitMs int64) float64 {
		var n int64
		for _, b := range h.Distribution() {
			if b.From >= limitMs*1000 {
				n += b.Count
			}
		}
		return float64(n) * 100 / float64(h.TotalCount())
	}
	fmt.Printf("requests=%d p99=%.1fms p99.9=%.1fms p99.99=%.1fms max=%.1fms >100ms=%.3f%% >500ms=%.3f%% >1s=%.4f%%\n",
		h.TotalCount(), ms(99), ms(99.9), ms(99.99), float64(h.Max())/1000, over(100), over(500), over(1000))
}
