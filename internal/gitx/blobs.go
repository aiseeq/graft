package gitx

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Blob is an object read through git cat-file --batch.
type Blob struct {
	Size int64
	// Head holds up to the first SniffLen bytes.
	Head []byte
	// Content is the full content, or nil when Size exceeds the read limit.
	Content []byte
}

// SniffLen is how much of a file git itself inspects to call it binary.
const SniffLen = 8000

// IsBinary applies git's heuristic: a NUL byte in the first 8000 bytes.
func (b Blob) IsBinary() bool {
	return bytes.IndexByte(b.Head, 0) >= 0
}

// ReadBlobs reads the objects named by specs in one git process. Blobs larger
// than maxContent keep only their Head. A spec that names no blob is an error.
func (r *Repo) ReadBlobs(specs []string, maxContent int64) ([]Blob, error) {
	if len(specs) == 0 {
		return []Blob{}, nil
	}
	for _, s := range specs {
		if strings.ContainsAny(s, "\n\r") {
			return nil, fmt.Errorf("object name %q contains a line break", s)
		}
	}
	cmd := r.Command("cat-file", "--batch")
	cmd.Stdin = strings.NewReader(strings.Join(specs, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("git cat-file stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting git cat-file: %w", err)
	}
	blobs, readErr := parseBatch(bufio.NewReader(stdout), specs, maxContent)
	if readErr != nil {
		// Unblock git before waiting for it; the read error is what matters.
		_, drainErr := io.Copy(io.Discard, stdout)
		return nil, errors.Join(readErr, drainErr, cmd.Wait())
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return blobs, nil
}

func parseBatch(r *bufio.Reader, specs []string, maxContent int64) ([]Blob, error) {
	blobs := make([]Blob, 0, len(specs))
	for _, spec := range specs {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", spec, err)
		}
		// "<oid> blob <size>", or "<spec> missing"
		fields := strings.Fields(header)
		if len(fields) == 2 && fields[1] == "missing" {
			return nil, fmt.Errorf("%s: no such object", spec)
		}
		if len(fields) != 3 || fields[1] != "blob" {
			return nil, fmt.Errorf("%s: unexpected cat-file header %q", spec, header)
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s: blob size: %w", spec, err)
		}
		b, err := readBlob(r, spec, size, maxContent)
		if err != nil {
			return nil, err
		}
		blobs = append(blobs, b)
	}
	return blobs, nil
}

func readBlob(r *bufio.Reader, spec string, size, maxContent int64) (Blob, error) {
	b := Blob{Size: size}
	if size <= maxContent {
		b.Content = make([]byte, size)
		if _, err := io.ReadFull(r, b.Content); err != nil {
			return b, fmt.Errorf("reading %s: %w", spec, err)
		}
		b.Head = b.Content[:min(size, SniffLen)]
	} else {
		b.Head = make([]byte, min(size, SniffLen))
		if _, err := io.ReadFull(r, b.Head); err != nil {
			return b, fmt.Errorf("reading %s: %w", spec, err)
		}
		if _, err := io.CopyN(io.Discard, r, size-int64(len(b.Head))); err != nil {
			return b, fmt.Errorf("reading %s: %w", spec, err)
		}
	}
	// The content is followed by a line feed.
	if _, err := r.ReadByte(); err != nil {
		return b, fmt.Errorf("reading %s: %w", spec, err)
	}
	return b, nil
}
