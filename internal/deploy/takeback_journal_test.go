package deploy

import (
	"bufio"
	"os/exec"
	"testing"
)

func TestTakeBackJournalReadsStderrOnlyAfterWait(t *testing.T) {
	cmd := exec.Command("sh", "-c", `
printf 'GRAFT_JOURNAL_READY\n'
IFS= read -r line || exit 2
i=0
while [ "$i" -lt 30000 ]; do
  printf 'stderr %s\n' "$i" >&2
  if [ "$i" -eq 0 ]; then exec 1>&-; fi
  i=$((i + 1))
done
cat >/dev/null
`)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	j := &TakeBackJournal{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), path: "test.journal"}
	cmd.Stderr = &j.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready, err := j.stdout.ReadString('\n')
	if err != nil || ready != "GRAFT_JOURNAL_READY\n" {
		t.Fatalf("journal opener handshake = %q, %v", ready, err)
	}
	if err := j.Write("sha", "reason"); err == nil {
		t.Fatal("Write succeeded after the opener closed its acknowledgement stream")
	}
	if err := j.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if j.stderr.Len() == 0 {
		t.Fatal("opener wrote no stderr for Close to collect")
	}
}
