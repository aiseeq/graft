package testreport

import (
	"os"
	"testing"
)

// TestParseJest: a jest --json report gives a run per test file, named
// relative to the step's directory; a test passed on a retry is flaky,
// pending and todo ones are skipped, and a file that did not run fails
// without tests.
func TestParseJest(t *testing.T) {
	data, err := os.ReadFile("testdata/jest.json")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := ParseJest(data, "/src/app")
	if err != nil {
		t.Fatal(err)
	}
	want := `src/__tests__/cart.test.tsx fail [["Cart adds an item",0.035,"pass"],["Cart totals sums prices",0.12,"fail"],["Cart reconnects",0.9,"flaky"],["Cart later",0,"skip"],["Cart some day",0,"skip"]]
src/__tests__/menu.test.tsx pass [["opens",0.012,"pass"]]
src/__tests__/broken.test.tsx fail null
`
	if got := summary(runs); got != want {
		t.Errorf("runs:\n%s\nwant\n%s", got, want)
	}
	if runs[0].WallS != 1.5 || runs[1].WallS != 0.3 {
		t.Errorf("file times: %v, %v", runs[0].WallS, runs[1].WallS)
	}
	if _, err := ParseJest([]byte(`{"testResults": "no"}`), "/src/app"); err == nil {
		t.Error("a report of another shape was accepted")
	}
}

func TestJestSkippedFilesAndRelativeNames(t *testing.T) {
	data := []byte(`{"testResults":[{"name":"/src/app/..cache/x.test.js","status":"skipped","startTime":1,"endTime":1},{"name":"/src/app/skip.test.js","status":"focused","startTime":1,"endTime":1}]}`)
	runs, err := ParseJest(data, "/src/app")
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].Package != "..cache/x.test.js" || runs[0].Result != "pass" || runs[1].Result != "pass" {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

func TestPlaywrightReportsFilesWithLoadErrors(t *testing.T) {
	data := []byte(`{"config":{"rootDir":"/src/app"},"suites":[],"errors":[{"location":{"file":"/src/app/broken.spec.ts"}}]}`)
	runs, err := ParsePlaywright(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Package != "broken.spec.ts" || runs[0].Result != "fail" || len(runs[0].Tests) != 0 {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

func TestPlaywrightReportsErrorsWithoutFileLocationAtRoot(t *testing.T) {
	data := []byte(`{"config":{"rootDir":"/src/app"},"suites":[],"errors":[{"message":"invalid config"}]}`)
	runs, err := ParsePlaywright(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Package != "." || runs[0].Result != "fail" || len(runs[0].Tests) != 0 {
		t.Fatalf("unexpected runs: %+v", runs)
	}
}

// TestParsePlaywright: a playwright JSON report gives a run per spec file;
// a test is named by its project, describes and title, takes the time of all
// its attempts, and is flaky when a retry passed it.
func TestParsePlaywright(t *testing.T) {
	data, err := os.ReadFile("testdata/playwright.json")
	if err != nil {
		t.Fatal(err)
	}
	runs, err := ParsePlaywright(data)
	if err != nil {
		t.Fatal(err)
	}
	want := `smoke/login.spec.ts pass [["[chromium] Login › accepts a valid password",0.243,"pass"],["[firefox] Login › accepts a valid password",0.31,"pass"],["[chromium] Login › locks after five tries",6.2,"flaky"],["[chromium] Login › remembers the user",0,"skip"]]
smoke/home.spec.ts fail [["[chromium] loads",30.7,"fail"]]
`
	if got := summary(runs); got != want {
		t.Errorf("runs:\n%s\nwant\n%s", got, want)
	}
	if runs[0].WallS != 6.753 || runs[1].WallS != 30.7 {
		t.Errorf("file times: %v, %v", runs[0].WallS, runs[1].WallS)
	}
	if _, err := ParsePlaywright([]byte(`{"stats": {}}`)); err == nil {
		t.Error("a report without suites was accepted")
	}
}
