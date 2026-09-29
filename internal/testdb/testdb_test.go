package testdb

import (
	"testing"

	"github.com/aiseeq/graft/internal/config"
)

func TestCheckDSN(t *testing.T) {
	cfg := &config.TestDB{Database: "app_test", DSNVar: "TEST_DB_DSN"}
	for dsn, ok := range map[string]bool{
		"postgres://u:p@127.0.0.1:5432/app_test?sslmode=disable": true,
		"postgresql://u@db/app_test":                             true,
		"postgres://u:p@127.0.0.1:5432/app":                      false,
		"postgres://u:p@127.0.0.1:5432/":                         false,
		"host=127.0.0.1 dbname=app_test":                         false,
		"mysql://u@h/app_test":                                   false,
	} {
		if err := CheckDSN(dsn, cfg); (err == nil) != ok {
			t.Errorf("CheckDSN(%q) = %v", dsn, err)
		}
	}
}

func TestDrifted(t *testing.T) {
	d := &DB{Cfg: &config.TestDB{Settings: []string{"fsync=off"}}}
	if d.Drifted(State{}) {
		t.Error("a missing container has not drifted")
	}
	for _, cmd := range [][]string{{"postgres", "-c", "fsync=off"}, {"-c", "fsync=off"}} {
		if d.Drifted(State{Exists: true, Cmd: cmd}) {
			t.Errorf("same settings %q reported as drift", cmd)
		}
	}
	if !d.Drifted(State{Exists: true, Cmd: []string{"postgres"}}) {
		t.Error("changed settings not reported")
	}
}
