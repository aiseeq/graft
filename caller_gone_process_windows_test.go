//go:build windows

package main

import "os"

func signalProcessZero(int) error {
	return os.ErrProcessDone
}
