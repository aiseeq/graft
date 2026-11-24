package tasks

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/aiseeq/graft/v2/internal/stats"
)

// Each step owns a journal. A nested graft reports its outer task to that
// journal; its own steps get a different journal, so each wait travels once
// through each level of the process tree.
const childWaitEnv = "GRAFT_CHILD_WAITS"

type childWaits struct{ path string }

func prepareChildWaits(cmd *exec.Cmd, log io.Writer) *childWaits {
	f, err := os.CreateTemp("", "graft-child-waits-*")
	if err != nil {
		fmt.Fprintf(log, "graft: cannot track nested waits: %v\n", err)
		return nil
	}
	_ = f.Close()
	w := &childWaits{path: f.Name()}
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	cmd.Env = append(cmd.Env, childWaitEnv+"="+w.path)
	return w
}

func (w *childWaits) close(log io.Writer) {
	if w == nil {
		return
	}
	if err := os.Remove(w.path); err != nil {
		fmt.Fprintf(log, "graft: cannot remove nested waits: %v\n", err)
	}
}

func reportChildWait(use stats.Usage, log io.Writer) {
	path := os.Getenv(childWaitEnv)
	if path == "" || use.Wait <= 0 {
		return
	}
	b, err := json.Marshal(use.WaitSpans)
	if err == nil {
		var f *os.File
		f, err = os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			_, err = f.Write(append(b, '\n'))
			err = errors.Join(err, f.Close())
		}
	}
	if err != nil {
		fmt.Fprintf(log, "graft: cannot report nested wait: %v\n", err)
	}
}

func (w *childWaits) addTo(use *stats.Usage, log io.Writer) {
	if w == nil {
		return
	}
	f, err := os.Open(w.path)
	if err != nil {
		fmt.Fprintf(log, "graft: cannot read nested waits: %v\n", err)
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4096), 8<<20)
	for sc.Scan() {
		var spans []stats.WaitSpan
		if err := json.Unmarshal(sc.Bytes(), &spans); err != nil {
			fmt.Fprintf(log, "graft: cannot read nested wait: %v\n", err)
			continue
		}
		use.WaitSpans = append(use.WaitSpans, spans...)
	}
	use.UnionWaits()
	if err := sc.Err(); err != nil {
		fmt.Fprintf(log, "graft: cannot read nested waits: %v\n", err)
	}
}
