// Package proc starts, probes and stops local processes portably.
package proc

// The exit statuses of a command that could not start, as env and a shell
// give them.
const (
	// ExitCannotRun is a command found but not started (not executable).
	ExitCannotRun = 126
	// ExitNotFound is a command not found.
	ExitNotFound = 127
)
