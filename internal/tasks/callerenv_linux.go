package tasks

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// parentEnv is the environment graft's parent process started with.
func parentEnv() ([]string, error) {
	path := "/proc/" + strconv.Itoa(os.Getppid()) + "/environ"
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading the caller's environment: %w", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00"), nil
}
