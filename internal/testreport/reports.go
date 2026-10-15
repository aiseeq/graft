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
		AssertionResults []struct {
			FullName    string
			Status      string
			Duration    *float64
			Invocations int
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
		if file.Status != "passed" {
			run.Result = stats.ResultFail
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
			run.Tests = append(run.Tests, res)
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// relative is name relative to dir, in slashes; a name outside dir stays
// as it is.
func relative(name, dir string) (string, error) {
	rel, err := filepath.Rel(dir, name)
	if err != nil {
		return "", fmt.Errorf("jest report: test file %s: %w", name, err)
	}
	if strings.HasPrefix(rel, "..") {
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

type playwrightSpec struct {
	Title string
	Tests []struct {
		ProjectName string
		Status      string
		Results     []struct {
			Duration float64
		}
	}
}

// ParsePlaywright reads a playwright JSON report: a run per spec file. A
// test is named "[project] describe › title", takes the time of all its
// attempts, and is flaky when a retry passed it.
func ParsePlaywright(data []byte) ([]stats.PackageRun, error) {
	var rep struct {
		Suites *[]playwrightSuite
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
	return runs, nil
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
