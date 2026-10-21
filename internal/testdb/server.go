package testdb

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// connect opens a connection to the configured database, the one the
// server is created with.
func (d *DB) connect(ctx context.Context) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, d.Cfg.DSN(d.Cfg.Database))
	if err != nil {
		return nil, fmt.Errorf("connecting to %s on %s: %w", d.Cfg.Container, d.Cfg.Where(), err)
	}
	return conn, nil
}

// query runs one statement and returns the rows as text.
func query(ctx context.Context, conn *pgx.Conn, sql string) ([][]string, error) {
	rows, err := conn.Query(ctx, sql, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", sql, err)
	}
	defer rows.Close()
	var got [][]string
	for rows.Next() {
		raw := rows.RawValues()
		row := make([]string, len(raw))
		for i, v := range raw {
			row[i] = string(v)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", sql, err)
	}
	return got, nil
}

// serverState is the state of a server docker may not be asked about, as it
// answers on the DSN: running once it listens where the config asks. While
// the image's entrypoint initializes a cluster, a temporary server answers
// on the socket with listen_addresses empty and then stops.
func (d *DB) serverState(ctx context.Context, r *RefusedError) (State, error) {
	s := State{Refused: r}
	ctx, cancel := context.WithTimeout(ctx, d.Cfg.ReadyTimeout)
	defer cancel()
	conn, err := d.connect(ctx)
	if err != nil {
		s.Unreachable = err
		return s, nil
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var listen string
	if err := conn.QueryRow(ctx, "SELECT current_setting('listen_addresses')").Scan(&listen); err != nil {
		return State{}, fmt.Errorf("asking %s where it listens: %w", d.Cfg.Container, err)
	}
	s.Exists, s.Running = true, listen != ""
	s.Settings = map[string]string{}
	for _, setting := range d.Cfg.Settings {
		name, _, _ := strings.Cut(setting, "=")
		var v string
		if err := conn.QueryRow(ctx, "SELECT current_setting($1)", name).Scan(&v); err != nil {
			return State{}, fmt.Errorf("asking %s for %s: %w", d.Cfg.Container, name, err)
		}
		s.Settings[name] = v
	}
	return s, nil
}

// settingsDrift lists the configured settings the server runs with other
// values of, as it reports them; a value spelled another way (1GB for
// 1024MB) counts as drift.
func (d *DB) settingsDrift(got map[string]string) string {
	var other []string
	for _, setting := range d.Cfg.Settings {
		name, want, _ := strings.Cut(setting, "=")
		if v := got[name]; !strings.EqualFold(v, strings.Trim(want, `'"`)) {
			other = append(other, name+"="+v)
		}
	}
	if len(other) == 0 {
		return ""
	}
	return "settings (" + strings.Join(other, " ") + ")"
}

// Unreachable is the error of a server that does not run where docker may
// not start it.
func (d *DB) Unreachable(s State) error {
	why := "is still initializing"
	if s.Unreachable != nil {
		why = "does not answer (" + s.Unreachable.Error() + ")"
	}
	return fmt.Errorf("%s %s, and %w; bring the database up outside the sandbox: graft testdb up", d.Cfg.Container, why, s.Refused)
}
