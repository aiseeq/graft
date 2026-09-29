package flags

import (
	"errors"
	"fmt"
	"strings"
)

// Event is one journal entry. Fields the source does not provide are empty.
type Event struct {
	ID        string
	Class     string
	Subject   string
	Body      string
	Status    string
	Severity  string
	Key       string
	Times     int
	FirstSeen string
	LastSeen  string
	Note      string
	NotedBy   string
	NotedAt   string
}

// ErrNotOpen means the event is missing or already closed.
var ErrNotOpen = errors.New("not found among open events (already closed?)")

// checkID rejects ids that cannot be a journal id; they would otherwise end
// up inside SQL literals, URLs and shell words.
func checkID(id string) error {
	if id == "" || len(id) > 200 || strings.ContainsAny(id, "'\"\\/ \t\r\n`$;|&<>") {
		return fmt.Errorf("%q is not a valid event id", id)
	}
	return nil
}
