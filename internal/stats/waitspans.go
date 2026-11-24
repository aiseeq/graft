package stats

import (
	"cmp"
	"slices"
	"time"
)

// WaitSpan is an interval spent waiting, with its cause.
type WaitSpan struct {
	Start time.Time
	End   time.Time
	Why   string
}

// NewWaitSpan constructs one measured interval.
func NewWaitSpan(start, end time.Time, why string) WaitSpan {
	return WaitSpan{Start: start, End: end, Why: why}
}

// UnionWaits counts overlapping intervals once, assigning each part to the
// first interval covering it. Keep the original spans for an enclosing task.
func (s *Usage) UnionWaits() {
	if len(s.WaitSpans) == 0 {
		return
	}
	spans := slices.Clone(s.WaitSpans)
	slices.SortFunc(spans, func(a, b WaitSpan) int {
		if c := a.Start.Compare(b.Start); c != 0 {
			return c
		}
		if c := a.End.Compare(b.End); c != 0 {
			return c
		}
		return cmp.Compare(a.Why, b.Why)
	})
	s.Wait = 0
	s.WaitWhy = make(map[string]time.Duration)
	var end time.Time
	for _, span := range spans {
		start := span.Start
		if start.Before(end) {
			start = end
		}
		if !span.End.After(start) {
			continue
		}
		d := span.End.Sub(start)
		s.Wait += d
		s.WaitWhy[span.Why] += d
		end = span.End
	}
}
