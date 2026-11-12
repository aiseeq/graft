//go:build !linux

package proc

// Refused: only Linux refuses programs to sandboxes this way.
func Refused(string) error { return nil }

// RefusedSudo: no_new_privs is a Linux process status.
func RefusedSudo() error { return nil }
