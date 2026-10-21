//go:build !linux

package testdb

// dockerRefused: only Linux has no_new_privs and SELinux.
func dockerRefused(string) (string, error) {
	return "", nil
}
