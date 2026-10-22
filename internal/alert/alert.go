// Package alert decides which threshold messages to send for a window.
// Delivery is deduped per window: the same threshold is not repeated until
// resets_at changes.
package alert

import "sort"

// Pending returns thresholds that used has reached and that were not sent yet
// for this window. A stale window produces nothing. Results are ascending.
func Pending(used float64, thresholds []int, sent map[int]bool, stale bool) []int {
	if stale || used < 0 {
		return nil
	}
	var out []int
	for _, th := range thresholds {
		if th <= 0 || th > 100 {
			continue
		}
		if sent != nil && sent[th] {
			continue
		}
		if used >= float64(th) {
			out = append(out, th)
		}
	}
	sort.Ints(out)
	return out
}

// Highest is the top pending threshold. A jump across several thresholds is
// one message; the caller marks every pending threshold as sent.
func Highest(pending []int) (int, bool) {
	if len(pending) == 0 {
		return 0, false
	}
	return pending[len(pending)-1], true
}
