package stats

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// FailureRuns returns failed steps and task failures that have no failed step
// in the same graft run. Newest records come first.

func FailureRuns(records []Record, project, task, in string) []*Record {
	failedSteps := map[string]bool{}
	taskInputs := map[string]bool{}
	for i := range records {
		r := &records[i]
		if r.Kind == KindStep && r.Exit != 0 {
			failedSteps[r.Run+"\x00"+r.Task] = true
		}
		if r.Kind == KindTask {
			taskInputs[r.Run+"\x00"+r.Task+"\x00"+r.In] = true
		}
	}
	var failures []*Record
	for i := range records {
		r := &records[i]
		if !failureMatches(r, project, task, in, failedSteps, taskInputs) {
			continue
		}
		failures = append(failures, r)
	}
	sort.SliceStable(failures, func(i, j int) bool { return failures[i].Time.After(failures[j].Time) })
	return failures
}

func failureMatches(r *Record, project, task, in string, failedSteps, taskInputs map[string]bool) bool {
	if r.Exit == 0 || (r.Kind != KindStep && r.Kind != KindTask) ||
		(project != "" && r.Project != project) || (task != "" && r.Task != task) {
		return false
	}
	key := r.Run + "\x00" + r.Task
	if r.Kind == KindTask {
		return !failedSteps[key] && (in == "" || r.In == in)
	}
	return in == "" || taskInputs[key+"\x00"+in]
}

// SameFailure describes a consecutive failure streak for the same tree and
// task, comparing the first non-empty line of the reason.
func SameFailure(records []Record, current Record) (time.Duration, int, bool) {
	first := firstReasonLine(current.Reason)
	if first == "" || current.Exit == 0 {
		return 0, 0, false
	}
	count := 0
	var previous time.Time
	for i := len(records) - 1; i >= 0; i-- {
		r := records[i]
		if r.Kind != KindTask || r.Project != current.Project || r.Task != current.Task {
			continue
		}
		if r.Exit == 0 || firstReasonLine(r.Reason) != first {
			break
		}
		count++
		if previous.IsZero() {
			previous = r.Time
		}
	}
	if count == 0 || previous.IsZero() {
		return 0, 0, false
	}
	return current.Time.Sub(previous), count + 1, true
}

func firstReasonLine(reason string) string {
	first, _, _ := strings.Cut(strings.TrimSpace(reason), "\n")
	return strings.TrimSpace(first)
}

// RepeatFailureLine formats the notice shown after a repeated failure.
func RepeatFailureLine(after time.Duration, count int) string {
	if after < 0 {
		after = 0
	}
	if after >= time.Minute {
		return fmt.Sprintf("same reason as %d min ago, %d times in a row", int(after.Round(time.Minute)/time.Minute), count)
	}
	return fmt.Sprintf("same reason as %d sec ago, %d times in a row", int(after.Round(time.Second)/time.Second), count)
}
