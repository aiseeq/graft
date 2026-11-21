package tasks

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/aiseeq/graft/v2/internal/config"
)

// These sections do not affect the gate's execution or local resource cost.
var lightGateKeyExcluded = []string{
	"version", "ticket", "push", "checks", "commit", "merge", "deploy",
	"release_notes", "release", "flags", "limits", "worktree",
}

func lightGateKey(cfg *config.Config) (string, error) {
	if cfg == nil {
		cfg = &config.Config{}
	}
	copy := *cfg
	value := reflect.ValueOf(&copy).Elem()
	t := value.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		tag := strings.Split(field.Tag.Get("yaml"), ",")[0]
		if tag != "" && tag != "-" && slices.Contains(lightGateKeyExcluded, tag) {
			value.Field(i).Set(reflect.Zero(field.Type))
		}
	}
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal gate config: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
