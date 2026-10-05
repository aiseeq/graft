package leaks

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Corpus is the vocabulary of public Go code: the identifiers of the Go
// distribution and of the modules in the module cache. A private project's
// name that public code also uses is a common name, not a leak.
type Corpus struct {
	words []string // sorted
}

// Has reports whether public code uses word.
func (c *Corpus) Has(word string) bool {
	_, ok := slices.BinarySearch(c.words, word)
	return ok
}

// corpusMinLen bounds the words the index keeps, below any sensible
// min_name_length, so a config change needs no new index.
const corpusMinLen = 6

// corpusWorkers bounds the parallel file reads of an index build.
const corpusWorkers = 4

// Index files in the cache directory: the module directories already read,
// and the sorted words.
const (
	corpusDirsFile  = "public-go-dirs.txt"
	corpusWordsFile = "public-go-words.txt"
)

// LoadCorpus reads the index in cacheDir and extends it with the module
// directories it has not read yet. Modules whose path starts with one of
// exclude (the user's own and private modules) are left out: their
// identifiers are the ones the check looks for.
func LoadCorpus(cacheDir string, exclude []string, log io.Writer) (*Corpus, error) {
	roots, err := corpusRoots(exclude)
	if err != nil {
		return nil, err
	}
	done, err := readLines(filepath.Join(cacheDir, corpusDirsFile))
	if err != nil {
		return nil, err
	}
	words, err := readLines(filepath.Join(cacheDir, corpusWordsFile))
	if err != nil {
		return nil, err
	}
	var todo []corpusRoot
	for _, r := range roots {
		if !slices.Contains(done, r.key) {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		return &Corpus{words: words}, nil
	}
	fmt.Fprintf(log, "graft: indexing the identifiers of %d public Go modules for the leak check (once per new module)\n", len(todo))
	found, err := scanDirs(todo)
	if err != nil {
		return nil, err
	}
	for _, w := range words {
		found[w] = true
	}
	merged := make([]string, 0, len(found))
	for w := range found {
		merged = append(merged, w)
	}
	slices.Sort(merged)
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the leak index directory: %w", err)
	}
	// Words first: a crash between the writes leaves directories to read
	// again, never directories counted without their words.
	if err := writeLines(filepath.Join(cacheDir, corpusWordsFile), merged); err != nil {
		return nil, err
	}
	for _, r := range todo {
		done = append(done, r.key)
	}
	if err := writeLines(filepath.Join(cacheDir, corpusDirsFile), done); err != nil {
		return nil, err
	}
	return &Corpus{words: merged}, nil
}

// corpusRoot is a directory of public Go code; key names it in the index.
type corpusRoot struct {
	key  string
	path string
}

// corpusRoots lists GOROOT/src and every module directory of the module
// cache, minus the excluded module paths.
func corpusRoots(exclude []string) ([]corpusRoot, error) {
	out, err := exec.Command("go", "env", "GOROOT", "GOMODCACHE").Output()
	if err != nil {
		return nil, fmt.Errorf("the leak check reads public Go code and needs the go command: go env: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 || lines[0] == "" || lines[1] == "" {
		return nil, fmt.Errorf("unexpected go env output %q", out)
	}
	goroot, modcache := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	version, err := os.ReadFile(filepath.Join(goroot, "VERSION"))
	if err != nil {
		return nil, fmt.Errorf("reading the Go version: %w", err)
	}
	// The version keys GOROOT: a new Go release is read again.
	first, _, _ := strings.Cut(string(version), "\n")
	src := filepath.Join(goroot, "src")
	roots := []corpusRoot{{key: src + "@" + strings.TrimSpace(first), path: src}}
	if _, err := os.Stat(modcache); errors.Is(err, os.ErrNotExist) {
		return roots, nil // no module downloaded yet
	}
	err = filepath.WalkDir(modcache, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(modcache, path)
		if err != nil {
			return err
		}
		if rel == "cache" {
			return filepath.SkipDir
		}
		if !strings.Contains(d.Name(), "@") {
			return nil
		}
		module, _, _ := strings.Cut(unescapeModule(filepath.ToSlash(rel)), "@")
		for _, ex := range exclude {
			if strings.HasPrefix(module, ex) {
				return filepath.SkipDir
			}
		}
		roots = append(roots, corpusRoot{key: path, path: path})
		return filepath.SkipDir
	})
	if err != nil {
		return nil, fmt.Errorf("listing the module cache: %w", err)
	}
	return roots, nil
}

// unescapeModule undoes the module cache's case escaping: !x is X.
func unescapeModule(p string) string {
	var b strings.Builder
	upper := false
	for _, r := range p {
		switch {
		case r == '!':
			upper = true
		case upper:
			b.WriteString(strings.ToUpper(string(r)))
			upper = false
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// scanDirs collects the words of the Go files under the roots.
func scanDirs(roots []corpusRoot) (map[string]bool, error) {
	files := make(chan string)
	results := make(chan map[string]bool)
	var mu sync.Mutex
	var errs []error
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		errs = append(errs, err)
	}
	var wg sync.WaitGroup
	for range corpusWorkers {
		wg.Go(func() {
			own := map[string]bool{}
			for path := range files {
				data, err := os.ReadFile(path)
				if err != nil {
					fail(err)
					continue
				}
				addWords(own, data)
			}
			results <- own
		})
	}
	go func() {
		defer close(files)
		for _, r := range roots {
			err := filepath.WalkDir(r.path, func(path string, e fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if e.Type().IsRegular() && strings.HasSuffix(path, ".go") {
					files <- path
				}
				return nil
			})
			if err != nil {
				fail(fmt.Errorf("reading %s: %w", r.path, err))
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	all := map[string]bool{}
	for own := range results {
		for w := range own {
			all[w] = true
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return all, nil
}

func addWords(into map[string]bool, data []byte) {
	for _, word := range wordRe.FindAll(data, -1) {
		for part := range bytes.SplitSeq(word, []byte("_")) {
			if distinctive(string(part), corpusMinLen) {
				into[string(part)] = true
			}
		}
	}
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the leak index: %w", err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if l := sc.Text(); l != "" {
			lines = append(lines, l)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return lines, nil
}

// writeLines replaces path atomically.
func writeLines(path string, lines []string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("writing the leak index: %w", err)
	}
	w := bufio.NewWriter(tmp)
	for _, l := range lines {
		if _, err := w.WriteString(l + "\n"); err != nil {
			return errors.Join(fmt.Errorf("writing %s: %w", tmp.Name(), err), tmp.Close(), os.Remove(tmp.Name()))
		}
	}
	if err := w.Flush(); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", tmp.Name(), err), tmp.Close(), os.Remove(tmp.Name()))
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("writing %s: %w", tmp.Name(), err), os.Remove(tmp.Name()))
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(fmt.Errorf("replacing %s: %w", path, err), os.Remove(tmp.Name()))
	}
	return nil
}
