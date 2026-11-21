package tasks

import (
	"reflect"
	"strings"
	"testing"

	"github.com/aiseeq/graft/v2/internal/config"
)

func TestLightGateKeyConfigFieldsAreClassified(t *testing.T) {
	excluded := map[string]bool{}
	for _, name := range lightGateKeyExcluded {
		excluded[name] = true
	}
	included := map[string]bool{}
	for _, name := range []string{
		"schema", "graft", "gate", "go_vet", "go_fix", "staticcheck", "go_fmt",
		"hooks", "lock", "envs", "dotenv", "dotenv_template", "dotenv_sets",
		"tasks", "cache_exclude", "services", "test_db", "tools", "remote_heavy", "MinGraft",
	} {
		included[name] = true
	}
	typ := reflect.TypeFor[config.Config]()
	for field := range typ.Fields() {
		if field.PkgPath != "" {
			continue
		}
		jsonName := strings.Split(field.Tag.Get("json"), ",")[0]
		if jsonName == "-" {
			continue
		}
		yamlName := strings.Split(field.Tag.Get("yaml"), ",")[0]
		name := yamlName
		if name == "" || name == "-" {
			name = jsonName
			if name == "" {
				name = field.Name
			}
		}
		if excluded[name] == included[name] {
			t.Errorf("Config.%s JSON field %q must be in exactly one classification", field.Name, name)
		}
		delete(excluded, name)
		delete(included, name)
	}
	for name := range excluded {
		t.Errorf("excluded yaml field %q does not exist on Config", name)
	}
	for name := range included {
		t.Errorf("included yaml field %q does not exist on Config", name)
	}
}

func TestLightGateKeyTracksOnlyGateRelevantConfig(t *testing.T) {
	key := func(cfg *config.Config) string {
		t.Helper()
		got, err := lightGateKey(cfg)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	base := &config.Config{
		Gate:   []config.Step{{Argv: []string{"go", "test"}}},
		Checks: config.Checks{},
		Tasks:  map[string]*config.Task{"inner": {Run: []config.Step{{Argv: []string{"go", "test"}}}}},
	}
	commentOnly := *base
	checksOnly := *base
	checksOnly.Checks.LargeFiles.Exceptions = []config.Exception{{Path: "vendor/**", Reason: "generated"}}
	if key(base) != key(&commentOnly) || key(base) != key(&checksOnly) {
		t.Fatal("excluded config changed the gate key")
	}
	taskChange := *base
	taskChange.Gate = []config.Step{{Argv: []string{"go", "test", "./..."}}}
	if key(base) == key(&taskChange) {
		t.Fatal("gate change did not change the key")
	}
	depChange := *base
	depChange.Tasks = map[string]*config.Task{"inner": {Deps: []string{"prepare"}, Run: []config.Step{{Argv: []string{"go", "test"}}}}}
	if key(base) == key(&depChange) {
		t.Fatal("nested task dependency change did not change the key")
	}
}
