package stats

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

type historyKey struct {
	Dev     uint64 `json:"dev"`
	Ino     uint64 `json:"ino"`
	Size    int64  `json:"size"`
	MtimeNS int64  `json:"mtime_ns"`
	CtimeNS int64  `json:"ctime_ns"`
	Edges   string `json:"sha256_edges"`
}

var errHistoryGZMiss = errors.New("history gzip cache miss")

func historyStat(f *os.File) (historyKey, error) {
	var key historyKey
	info, err := f.Stat()
	if err != nil {
		return key, fmt.Errorf("stating history file: %w", err)
	}
	stat := reflect.ValueOf(info.Sys())
	if stat.Kind() == reflect.Pointer {
		stat = stat.Elem()
	}
	if stat.Kind() != reflect.Struct {
		return key, errors.New("unsupported file stat")
	}
	dev, okDev := statFieldUint(stat, "Dev")
	ino, okIno := statFieldUint(stat, "Ino")
	mtime, okMtime := statFieldNanos(stat, "Mtim", "Mtimespec")
	ctime, okCtime := statFieldNanos(stat, "Ctim", "Ctimespec")
	if !okDev || !okIno || !okMtime || !okCtime {
		return key, errors.New("unsupported file stat")
	}
	key.Dev, key.Ino, key.Size = dev, ino, info.Size()
	key.MtimeNS, key.CtimeNS = mtime, ctime
	var edges [8192]byte
	n := min(key.Size, int64(4096))
	if n > 0 {
		if _, err := f.ReadAt(edges[:n], 0); err != nil && !errors.Is(err, io.EOF) {
			return key, err
		}
		if key.Size > n {
			if _, err := f.ReadAt(edges[n:n*2], key.Size-n); err != nil && !errors.Is(err, io.EOF) {
				return key, err
			}
		}
	}
	length := n
	if key.Size > 4096 {
		length = 8192
	}
	sum := sha256.Sum256(edges[:length])
	key.Edges = hex.EncodeToString(sum[:])
	return key, nil
}

func statFieldUint(stat reflect.Value, name string) (uint64, bool) {
	field := stat.FieldByName(name)
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		value := field.Int()
		return uint64(value), value >= 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return field.Uint(), true
	default:
		return 0, false
	}
}

func statFieldNanos(stat reflect.Value, names ...string) (int64, bool) {
	for _, name := range names {
		field := stat.FieldByName(name)
		if !field.IsValid() || field.Kind() != reflect.Struct {
			continue
		}
		sec, okSec := statNumber(field.FieldByName("Sec"))
		nsec, okNsec := statNumber(field.FieldByName("Nsec"))
		if okSec && okNsec {
			return sec*1e9 + nsec, true
		}
	}
	return 0, false
}

func statNumber(field reflect.Value) (int64, bool) {
	if !field.IsValid() {
		return 0, false
	}
	switch field.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return field.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int64(field.Uint()), true
	default:
		return 0, false
	}
}

func sameHistoryKey(a, b historyKey) bool { return a == b }

func rotated(path string) bool {
	base := filepath.Base(path)
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 || dot == len(base)-1 {
		return false
	}
	n, err := strconv.Atoi(base[dot+1:])
	return err == nil && n > 0
}

func historyGZPath(path string, key historyKey) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("locating user cache: %w", err)
	}
	name := filepath.Base(path)
	dot := strings.LastIndexByte(name, '.')
	if dot <= 0 {
		return "", fmt.Errorf("invalid rotated history name %q", name)
	}
	name = name[:dot]
	return filepath.Join(cache, "graft", "history-gz", fmt.Sprintf("%s-%d-%d.gz", name, key.Dev, key.Ino)), nil
}

// readHistory parses a report file, accepting a gzip derivative only after
// its checksum and the source file's identity have both been verified.
func readHistory[T any](path string, decode func(io.Reader) (T, int, error)) (T, int, error) {
	return readHistoryWithHook(path, decode, nil)
}

func readHistoryWithHook[T any](path string, decode func(io.Reader) (T, int, error), afterGZInflate func()) (T, int, error) {
	var zero T
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return zero, 0, nil
	}
	if err != nil {
		return zero, 0, fmt.Errorf("reading the stats: %w", err)
	}
	defer f.Close()
	if !rotated(path) {
		v, bad, err := decode(f)
		if err != nil {
			return zero, 0, fmt.Errorf("reading %s: %w", path, err)
		}
		return v, bad, nil
	}

	key, keyErr := historyStat(f)
	gzPath, cacheErr := historyGZPath(path, key)
	if keyErr == nil && cacheErr == nil {
		if gz, openErr := os.Open(gzPath); openErr == nil {
			v, bad, gzErr := readValidGZWithHook(gz, f, key, decode, afterGZInflate)
			gz.Close()
			if gzErr == nil {
				return v, bad, nil
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			warnHistory("opening", gzPath, openErr)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return zero, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	v, bad, err := decode(f)
	if err != nil {
		return zero, 0, fmt.Errorf("reading %s: %w", path, err)
	}
	if keyErr == nil && cacheErr == nil {
		writeHistoryGZ(f, gzPath, key)
	}
	return v, bad, nil
}

func readValidGZ[T any](gz *os.File, source *os.File, key historyKey, decode func(io.Reader) (T, int, error)) (T, int, error) {
	return readValidGZWithHook(gz, source, key, decode, nil)
}

func readValidGZWithHook[T any](gz *os.File, source *os.File, key historyKey, decode func(io.Reader) (T, int, error), afterInflate func()) (T, int, error) {
	var zero T
	gr, err := gzip.NewReader(gz)
	if err != nil {
		return zero, 0, fmt.Errorf("opening history gzip: %w", err)
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		closeErr := gr.Close()
		return zero, 0, fmt.Errorf("encoding history key: %w", errors.Join(err, closeErr))
	}
	if gr.Comment != string(encoded) {
		if err := gr.Close(); err != nil {
			return zero, 0, fmt.Errorf("closing mismatched history gzip: %w", err)
		}
		return zero, 0, errHistoryGZMiss
	}
	maxInt := int(^uint(0) >> 1)
	if key.Size < 0 || uint64(key.Size) >= uint64(maxInt) {
		closeErr := gr.Close()
		return zero, 0, fmt.Errorf("source size %d cannot be buffered: %w", key.Size, errors.Join(errHistoryGZMiss, closeErr))
	}
	buffer := bytes.NewBuffer(make([]byte, 0, int(key.Size)))
	_, readErr := io.Copy(buffer, io.LimitReader(gr, key.Size+1))
	closeErr := gr.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return zero, 0, fmt.Errorf("reading and checking history gzip: %w", err)
	}
	data := buffer.Bytes()
	if int64(len(data)) != key.Size {
		return zero, 0, fmt.Errorf("gzip payload size %d differs from source size %d: %w", len(data), key.Size, errHistoryGZMiss)
	}
	if afterInflate != nil {
		afterInflate()
	}
	current, err := historyStat(source)
	if err != nil {
		return zero, 0, fmt.Errorf("checking source history key: %w", err)
	}
	if !sameHistoryKey(current, key) {
		return zero, 0, errHistoryGZMiss
	}
	v, bad, err := decode(bytes.NewReader(data))
	if err != nil {
		return zero, 0, fmt.Errorf("decoding verified history gzip: %w", err)
	}
	return v, bad, nil
}

func writeHistoryGZ(source *os.File, path string, key historyKey) {
	before, err := historyStat(source)
	if err != nil {
		warnHistory("stat", path, err)
		return
	}
	if !sameHistoryKey(before, key) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		warnHistory("creating", filepath.Dir(path), err)
		return
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		warnHistory("encoding", path, err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".history-*.tmp")
	if err != nil {
		warnHistory("creating", path, err)
		return
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	gz := gzip.NewWriter(tmp)
	gz.Comment = string(encoded)
	if _, err = source.Seek(0, io.SeekStart); err == nil {
		_, err = io.Copy(gz, source)
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	if syncErr := tmp.Sync(); err == nil {
		err = syncErr
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	after, statErr := historyStat(source)
	if err == nil && statErr != nil {
		err = statErr
	}
	if err == nil && !sameHistoryKey(before, after) {
		err = errors.New("source changed while compressing")
	}
	if err == nil {
		err = os.Rename(tmpPath, path)
	}
	if err != nil {
		warnHistory("writing", path, err)
	}
}

func warnHistory(action, path string, err error) {
	fmt.Fprintf(os.Stderr, "graft: history gzip %s %s: %v\n", action, path, err)
}

// sweepHistoryGZ keeps derivatives only for currently existing stats/tests copies.
func sweepHistoryGZ(statsPath string) {
	paths := []string{statsPath, TestsPath(statsPath)}
	keep := map[string]bool{}
	for _, p := range paths {
		copies, err := oldCopies(p)
		if err != nil {
			warnHistory("listing", p, err)
			continue
		}
		for _, c := range copies {
			f, err := os.Open(c.path)
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					warnHistory("opening", c.path, err)
				}
				continue
			}
			key, statErr := historyStat(f)
			f.Close()
			if statErr != nil {
				warnHistory("stat", c.path, statErr)
				continue
			}
			gzPath, pathErr := historyGZPath(c.path, key)
			if pathErr != nil {
				warnHistory("locating", c.path, pathErr)
			} else {
				keep[filepath.Base(gzPath)] = true
			}
		}
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		warnHistory("locating", "history-gz", err)
		return
	}
	dir := filepath.Join(cache, "graft", "history-gz")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		warnHistory("listing", dir, err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".gz") || keep[e.Name()] {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			warnHistory("removing", filepath.Join(dir, e.Name()), err)
		}
	}
}
