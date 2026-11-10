package workers

import "testing"

func TestListedFilesFiltersExcludedPaths(t *testing.T) {
	staged := map[string]string{"projectA.txt": "blob-a", "go.mod": "blob-b", "kept.txt": "blob-c"}
	files := map[string]string{"projectA.txt": "blob-a", "go.mod": "blob-b", "kept.txt": "blob-c"}
	if got := listedFiles(files, staged, nil, []string{"projectA.txt"}); got != 1 {
		t.Errorf("listed staged files = %d, want only the non-excluded file", got)
	}
}
