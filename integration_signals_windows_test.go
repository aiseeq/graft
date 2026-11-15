//go:build windows

package main

func watchTestSignals() func() { return func() {} }
