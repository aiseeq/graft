package deploy

import (
	"slices"
	"strings"
	"time"

	"github.com/aiseeq/graft/internal/config"
)

// Occurrence is one release window: its day and moments.
type Occurrence struct {
	// Date is the window's day, YYYY-MM-DD in the window's zone.
	Date               string
	Cutoff, Start, End time.Time
}

// Name is the window's day as DD.MM.YY, the way release names carry it.
func (o Occurrence) Name() string {
	return o.Start.Format("02.01.06")
}

// occurrence is the window on the day of t in the window's zone, if that day
// has one.
func occurrence(w *config.Window, t time.Time) (Occurrence, bool) {
	local := t.In(w.Location)
	if !slices.Contains(w.Weekdays, local.Weekday()) {
		return Occurrence{}, false
	}
	day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, w.Location)
	o := Occurrence{Date: day.Format(time.DateOnly), Start: clock(day, w.Start), End: clock(day, w.End)}
	if w.Cutoff != nil {
		o.Cutoff = clock(day.AddDate(0, 0, -w.Cutoff.DaysBefore), w.Cutoff.Time)
	}
	return o, true
}

// clock is the wall-clock offset on the day: a day of a daylight saving
// change is not 24 hours long.
func clock(day time.Time, offset time.Duration) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), int(offset/time.Hour), int(offset%time.Hour/time.Minute), 0, 0, day.Location())
}

// Open tells whether a window is open at now.
func Open(w *config.Window, now time.Time) bool {
	o, ok := occurrence(w, now)
	return ok && !now.Before(o.Start) && now.Before(o.End)
}

// Next is the window open at now or the first one after it.
func Next(w *config.Window, now time.Time) Occurrence {
	day := now.In(w.Location)
	for range 8 {
		if o, ok := occurrence(w, day); ok && now.Before(o.End) {
			return o
		}
		day = day.AddDate(0, 0, 1)
	}
	panic("a window with days has one within a week")
}

// Hold returns the window whose candidate is held at now: its cutoff has
// passed and it has not ended.
func Hold(w *config.Window, now time.Time) (Occurrence, bool) {
	if w.Cutoff == nil {
		return Occurrence{}, false
	}
	day := now.In(w.Location)
	for range 8 + w.Cutoff.DaysBefore {
		if o, ok := occurrence(w, day); ok && now.Before(o.End) {
			return o, !now.Before(o.Cutoff)
		}
		day = day.AddDate(0, 0, 1)
	}
	return Occurrence{}, false
}

// Describe prints the window as its config says it.
func Describe(w *config.Window) string {
	return strings.Join(w.Days, ", ") + " " + w.At + "-" + w.Until + " " + w.TZ
}
