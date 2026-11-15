package stats

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// LoadGateHistory loads the records needed by LightGate and SplitLightGate.
// A nested graft may change projects, so a project's records alone are not
// enough: the first pass connects runs through parent links in every project.
// The second pass decodes the project's records and those connected runs.
// Malformed records are ignored as in Load; unlike Load their number is not
// returned, since skipped records are deliberately not decoded.
func LoadGateHistory(path, project string, since time.Time) ([]Record, error) {
	return loadGateHistory(path, project, since, nil)
}

func loadGateHistory(path, project string, since time.Time, betweenPasses func()) ([]Record, error) {
	files, err := Files(path, since)
	if err != nil {
		return nil, err
	}
	snapshot, err := openGateFiles(files)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, f := range snapshot {
			_ = f.file.Close()
		}
	}()
	encoded, err := json.Marshal(project)
	if err != nil {
		return nil, fmt.Errorf("encoding project for gate history: %w", err)
	}
	var unescaped bytes.Buffer
	enc := json.NewEncoder(&unescaped)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(project); err != nil {
		return nil, fmt.Errorf("encoding project for gate history: %w", err)
	}
	plain := bytes.TrimSpace(unescaped.Bytes())
	sameEncoding := bytes.Equal(plain, encoded)
	mentionsProject := func(line []byte, unusual bool) bool {
		return bytes.Contains(line, encoded) || !sameEncoding && bytes.Contains(line, plain) || unusual
	}
	roots, err := gateHistoryRoots(snapshot, project, since, mentionsProject)
	if err != nil {
		return nil, err
	}
	if betweenPasses != nil {
		betweenPasses()
	}

	var recs []Record
	err = scanGateFiles(snapshot, func(line []byte) {
		unusual := unusualJSON(line)
		if !mentionsProject(line, unusual) && !gateRunMatches(line, roots, unusual) {
			return
		}
		var rec Record
		if json.Unmarshal(line, &rec) == nil && rec.Kind != "" && rec.Task != "" {
			recs = append(recs, rec)
		}
	})
	return recs, err
}

// gateHistoryRoots includes the connected component of every relevant gate
// run. Links from malformed lines can only expand the component.
func gateHistoryRoots(files []gateFile, project string, since time.Time, mentionsProject func([]byte, bool) bool) (map[string]bool, error) {
	links := make(map[string][]string)
	roots := make(map[string]bool)
	err := scanGateFiles(files, func(line []byte) {
		unusual := unusualJSON(line)
		if mentionsProject(line, unusual) {
			var rec Record
			if json.Unmarshal(line, &rec) == nil && isGateHistoryRoot(rec, project, since) {
				roots[rec.Run] = true
			}
		}
		run, parent, fast := gateEdge(line, unusual)
		if !fast {
			var edge struct {
				Run    string `json:"run"`
				Parent string `json:"parent"`
			}
			if json.Unmarshal(line, &edge) == nil {
				run, parent = edge.Run, edge.Parent
			}
		}
		if run != "" && parent != "" {
			links[run] = append(links[run], parent)
			links[parent] = append(links[parent], run)
		}
	})
	if err != nil {
		return nil, err
	}
	queue := make([]string, 0, len(roots))
	for id := range roots {
		queue = append(queue, id)
	}
	for i := 0; i < len(queue); i++ {
		for _, id := range links[queue[i]] {
			if !roots[id] {
				roots[id] = true
				queue = append(queue, id)
			}
		}
	}
	return roots, nil
}

func isGateHistoryRoot(rec Record, project string, since time.Time) bool {
	return rec.Kind == KindTask && rec.Task == GateTask && rec.Project == project && rec.Parent == "" && rec.Steps > 0 && rec.Time.After(since) && rec.Run != ""
}

// unusualJSON says whether an escape could change a field used by the
// selection. Escapes in unrelated values such as a failure reason do not
// require decoding the entire line. An escaped key always takes the decoder.
func unusualJSON(line []byte) bool {
	if !gateHasJSONEscape(line) {
		return false
	}
	depth := 0
	for i := 0; i < len(line); {
		switch line[i] {
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			i++
		case '"':
			end := gateStringEnd(line, i+1)
			if end < 0 {
				return true
			}
			if depth == 1 && gateEscapedField(line, i+1, end) {
				return true
			}
			i = end + 1
		default:
			i++
		}
	}
	return false
}

// gateEscapedField checks a top-level JSON key and its selection-relevant value.
func gateEscapedField(line []byte, start, end int) bool {
	next := gateSkipSpace(line, end+1)
	if next >= len(line) || line[next] != ':' {
		return false
	}
	key := line[start:end]
	if bytes.ContainsRune(key, '\\') {
		return true
	}
	if !bytes.EqualFold(key, []byte("project")) && !bytes.EqualFold(key, []byte("run")) && !bytes.EqualFold(key, []byte("parent")) {
		return false
	}
	next = gateSkipSpace(line, next+1)
	if next >= len(line) {
		return true
	}
	if line[next] != '"' {
		return false
	}
	valueEnd := gateStringEnd(line, next+1)
	if valueEnd < 0 {
		return true
	}
	return gateHasJSONEscape(line[next+1 : valueEnd])
}

func gateSkipSpace(line []byte, i int) int {
	for i < len(line) && (line[i] == ' ' || line[i] == '\t' || line[i] == '\r' || line[i] == '\n') {
		i++
	}
	return i
}

// gateHasJSONEscape finds escapes that can change JSON keys or values.
func gateHasJSONEscape(line []byte) bool {
	for len(line) > 1 {
		i := bytes.IndexByte(line, '\\')
		if i < 0 || i+1 == len(line) {
			return false
		}
		if line[i+1] == 'u' || line[i+1] == '/' {
			return true
		}
		line = line[i+1:]
	}
	return false
}

// gateStringEnd finds the end of a JSON string without interpreting it.
func gateStringEnd(line []byte, start int) int {
	for start < len(line) {
		n := bytes.IndexByte(line[start:], '"')
		if n < 0 {
			return -1
		}
		end := start + n
		slashes := 0
		for j := end - 1; j >= 0 && line[j] == '\\'; j-- {
			slashes++
		}
		if slashes%2 == 0 {
			return end
		}
		start = end + 1
	}
	return -1
}

// hasFold finds a JSON key in any casing encoding/json accepts. A false
// positive inside a value is harmless: it only makes the decoder look at it.
func hasFold(line []byte, key string) bool {
	first := key[1] | 32
	chars := "rR"
	if first == 'p' {
		chars = "pP"
	}
	for len(line) >= len(key) {
		n := bytes.IndexAny(line, chars)
		if n < 0 {
			return false
		}
		if n > 0 && n-1+len(key) <= len(line) && bytes.EqualFold(line[n-1:n-1+len(key)], []byte(key)) {
			return true
		}
		line = line[n+1:]
	}
	return false
}

// gateEdge reads canonical run/parent strings without decoding unrelated
// fields. Invalid records can add links, which only admits extra records;
// the second pass still validates every record it returns.
func gateEdge(line []byte, unusual bool) (string, string, bool) {
	if unusual {
		return "", "", false
	}
	const parentKey = `"parent":"`
	const runKey = `"run":"`
	p := bytes.Index(line, []byte(parentKey))
	if p < 0 {
		return "", "", !hasFold(line, `"parent"`)
	}
	r := bytes.Index(line, []byte(runKey))
	if r < 0 {
		return "", "", false
	}
	value := func(at int, key string) (string, bool) {
		v := line[at+len(key):]
		n := bytes.IndexByte(v, '"')
		if n < 0 || bytes.ContainsRune(v[:n], '\\') || hasFold(v[n+1:], key[:len(key)-2]) {
			return "", false
		}
		return string(v[:n]), true
	}
	run, ok := value(r, runKey)
	if !ok {
		return "", "", false
	}
	parent, ok := value(p, parentKey)
	if !ok {
		return "", "", false
	}
	return run, parent, true
}

func gateRunMatches(line []byte, runs map[string]bool, unusual bool) bool {
	if len(runs) == 0 {
		return false
	}
	// The journal writer emits the run at top level in this form. For other
	// spellings, duplicate keys or escapes, let encoding/json interpret it.
	const field = `"run":"`
	_, value, ok := bytes.Cut(line, []byte(field))
	if ok && !unusual {
		end := bytes.IndexByte(value, '"')
		if end >= 0 && !bytes.ContainsRune(value[:end], '\\') && !hasFold(value[end+1:], `"run"`) {
			return runs[string(value[:end])]
		}
	}
	if !hasFold(line, `"run"`) && !unusual {
		return false
	}
	var rec struct {
		Run string `json:"run"`
	}
	return json.Unmarshal(line, &rec) == nil && runs[rec.Run]
}

type gateFile struct {
	file *os.File
	size int64
}

func openGateFiles(files []string) ([]gateFile, error) {
	var opened []gateFile
	for _, path := range files {
		f, err := os.Open(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			for _, prev := range opened {
				_ = prev.file.Close()
			}
			return nil, fmt.Errorf("reading the stats: %w", err)
		}
		st, err := f.Stat()
		if err != nil {
			_ = f.Close()
			for _, prev := range opened {
				_ = prev.file.Close()
			}
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		opened = append(opened, gateFile{file: f, size: st.Size()})
	}
	return opened, nil
}

func scanGateFiles(files []gateFile, visit func([]byte)) error {
	for _, f := range files {
		sc := bufio.NewScanner(io.NewSectionReader(f.file, 0, f.size))
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			visit(sc.Bytes())
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("reading %s: scanning stats records: %w", f.file.Name(), err)
		}
	}
	return nil
}
