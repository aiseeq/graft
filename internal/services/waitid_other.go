//go:build !linux

package services

func serviceExitedWithoutStart(int) (bool, error) { return false, nil }
