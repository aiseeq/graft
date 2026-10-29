package testreport

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/aiseeq/graft/v2/internal/stats"
)

// jestReport is the part of a jest --json report graft reads.
type jestReport struct {
	TestResults *[]struct {
		Name             string
		Status           string
		StartTime        int64
		EndTime          int64
		FailureMessage   string
		AssertionResults []struct {
			FullName        string
			Status          string
			Duration        *float64
			Invocations     int
			FailureMessages []string
		}
	}
}

// ParseJest reads a jest --json report: a run per test file, named relative
// to dir, the directory jest ran in. A test passed on a retry is flaky;
// pending, todo and skipped ones are skipped.
func ParseJest(data []byte, dir string) ([]stats.PackageRun, error) {
	var rep jestReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("a jest --json report: %w", err)
	}
	if rep.TestResults == nil {
		return nil, errors.New("a jest --json report has testResults")
	}
	var runs []stats.PackageRun
	for _, file := range *rep.TestResults {
		pkg, err := relative(file.Name, dir)
		if err != nil {
			return nil, err
		}
		run := newRun(pkg, stats.ResultPass, float64(file.EndTime-file.StartTime)/1000)
		if file.Status == "failed" {
			run.Result = stats.ResultFail
			run.Reason = stats.LimitReason(file.FailureMessage)
		}
		for _, a := range file.AssertionResults {
			res := stats.TestResult{Name: a.FullName}
			if a.Duration != nil {
				res.WallS = round(*a.Duration / 1000)
			}
			switch a.Status {
			case "passed":
				res.Result = stats.ResultPass
				if a.Invocations > 1 {
					res.Result = stats.ResultFlaky
				}
			case "failed":
				res.Result = stats.ResultFail
			case "pending", "todo", "skipped", "disabled", "focused":
				res.Result = stats.ResultSkip
			default:
				return nil, fmt.Errorf("jest report: %s: test %q has status %q", file.Name, a.FullName, a.Status)
			}
			if res.Result == stats.ResultFail {
				res.Reason = stats.LimitReason(strings.Join(a.FailureMessages, "\n"))
				if run.Reason == "" {
					run.Reason = res.Reason
				}
			}
			run.Tests = append(run.Tests, res)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// firstNonEmpty returns the first string containing non-whitespace text.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// relative is name relative to dir, in slashes; a name outside dir stays
// as it is.
func relative(name, dir string) (string, error) {
	if filepath.VolumeName(name) != filepath.VolumeName(dir) {
		return filepath.ToSlash(name), nil
	}
	rel, err := filepath.Rel(dir, name)
	if err != nil {
		return "", fmt.Errorf("jest report: test file %s: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(name), nil
	}
	return filepath.ToSlash(rel), nil
}

type playwrightSuite struct {
	Title  string
	File   string
	Specs  []playwrightSpec
	Suites []playwrightSuite
}

type playwrightError struct {
	Message  string
	Stack    string
	Location struct {
		File string
	}
}

type playwrightSpec struct {
	Title string
	Tests []struct {
		ProjectName string
		Status      string
		Results     []playwrightResult
	}
}

type playwrightResult struct {
	Duration float64
	Error    *struct {
		Message string
		Stack   string
	}
}

// ParsePlaywright reads a playwright JSON report: a run per spec file. A
// test is named "[project] describe › title", takes the time of all its
// attempts, and is flaky when a retry passed it.
func ParsePlaywright(data []byte) ([]stats.PackageRun, error) {
	var rep struct {
		Suites *[]playwrightSuite
		Config struct{ RootDir string }
		Errors []playwrightError
	}
	if err := json.Unmarshal(data, &rep); err != nil {
		return nil, fmt.Errorf("a playwright JSON report: %w", err)
	}
	if rep.Suites == nil {
		return nil, errors.New("a playwright JSON report has suites")
	}
	var runs []stats.PackageRun
	for _, file := range *rep.Suites {
		run := newRun(file.File, stats.ResultPass, 0)
		if err := playwrightTests(&run, file, nil); err != nil {
			return nil, err
		}
		run.WallS = round(run.WallS)
		runs = append(runs, run)
	}
	for _, e := range rep.Errors {
		pkg := "."
		if e.Location.File != "" {
			pkg = filepath.ToSlash(e.Location.File)
			if rep.Config.RootDir != "" {
				if rel, err := filepath.Rel(rep.Config.RootDir, e.Location.File); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					pkg = filepath.ToSlash(rel)
				}
			}
		}
		run := newRun(pkg, stats.ResultFail, 0)
		run.Reason = stats.LimitReason(firstNonEmpty(e.Message, e.Stack))
		runs = append(runs, run)
	}
	return runs, nil
}

func playwrightReason(results []playwrightResult) string {
	var reasons []string
	for _, result := range results {
		if result.Error != nil {
			reasons = append(reasons, firstNonEmpty(result.Error.Message, result.Error.Stack))
		}
	}
	return stats.LimitReason(strings.Join(reasons, "\n"))
}

// playwrightTests adds the tests of suite to run; titles are the describes
// above, the file's own suite left out.
func playwrightTests(run *stats.PackageRun, suite playwrightSuite, titles []string) error {
	for _, spec := range suite.Specs {
		name := strings.Join(append(append([]string{}, titles...), spec.Title), " › ")
		for _, t := range spec.Tests {
			res := stats.TestResult{Name: name}
			if t.ProjectName != "" {
				res.Name = "[" + t.ProjectName + "] " + name
			}
			for _, r := range t.Results {
				res.WallS += r.Duration / 1000
			}
			res.WallS = round(res.WallS)
			run.WallS += res.WallS
			switch t.Status {
			case "expected":
				res.Result = stats.ResultPass
			case "unexpected":
				res.Result = stats.ResultFail
				run.Result = stats.ResultFail
				res.Reason = playwrightReason(t.Results)
				if run.Reason == "" {
					run.Reason = res.Reason
				}
			case "flaky":
				res.Result = stats.ResultFlaky
			case "skipped":
				res.Result = stats.ResultSkip
			default:
				return fmt.Errorf("playwright report: %s: test %q has status %q", run.Package, res.Name, t.Status)
			}
			run.Tests = append(run.Tests, res)
		}
	}
	for _, child := range suite.Suites {
		if err := playwrightTests(run, child, append(titles, child.Title)); err != nil {
			return err
		}
	}
	return nil
}
