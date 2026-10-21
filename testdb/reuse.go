package testdb

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A copy a test is done with goes back to its binary's pool once its data is
// put back as the template had it, which costs a few times less than a new
// copy: the next test of the binary takes it. Only the data is put back: a
// test that changed the schema, the planner statistics or anything else the
// reset cannot see through leaves its copy to be dropped, as every copy was
// before.
//
// What a test changed comes from the cumulative statistics, flushed when the
// test's connections end: the tables with rows inserted, updated or deleted,
// and the tables whose files changed (TRUNCATE leaves no row counts). Those
// are emptied with the foreign keys off, the template's rows copied back, the
// tables vacuumed and analyzed, and the sequences set back. The tables the
// test did not write keep the template's rows, which reference the template's
// rows of the others, put back.

// reuseSince is the server_version_num of PostgreSQL 15, which added
// pg_stat_force_next_flush: the reset's own changes must reach the statistics
// before they are cleared.
const reuseSince = 150000

// pooled is a copy of the template with what its reset compares against.
type pooled struct {
	name string
	// template is the oid of the template the copy was made from: a template
	// rebuilt since has another snapshot.
	template uint32
	// filenodes are the files of the user tables when the copy was handed
	// out, by table oid.
	filenodes map[uint32]uint32
	// keeper is the reset's connection to the copy, kept while the copy
	// lives: a new connection reads the catalogs into its caches anew, which
	// costs more than the reset itself.
	keeper *pgx.Conn
}

// snapshot is what a fresh copy of the template holds that a test may change
// with the schema left alone, read once per binary from the template.
type snapshot struct {
	template uint32
	// classes is a digest of pg_class without the fields that change with
	// the data: a TRUNCATE gives a table new files and nothing else.
	classes   string
	filenodes map[uint32]uint32
	// seeds are the rows of the tables that have any, as COPY ... TO STDOUT
	// (FORMAT binary) gives them, by table oid.
	seeds     map[uint32][]byte
	sequences map[string]sequenceState
}

type sequenceState struct {
	// last is nil for a sequence nextval was never called on.
	last  *int64
	start int64
}

const classesDigest = `SELECT md5(string_agg(row(oid, relname, relnamespace, reltype, reloftype, relowner, relam,
	reltablespace, reltoastrelid, relhasindex, relpersistence, relkind, relnatts, relchecks, relhasrules,
	relhastriggers, relhassubclass, relrowsecurity, relforcerowsecurity, relispopulated, relreplident,
	relispartition, relacl, reloptions, relpartbound)::text, ',' ORDER BY oid)) FROM pg_class`

// userTables are the plain tables and materialized views outside the system
// schemas, with their names and files; temporary tables end with their
// sessions.
const userTables = `SELECT c.oid, c.oid::regclass::text, c.relkind = 'm', c.relfilenode FROM pg_class c
	JOIN pg_namespace n ON n.oid = c.relnamespace
	WHERE c.relkind IN ('r', 'm') AND c.relpersistence <> 't'
	AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg\_toast%'`

// userTable is a row of userTables.
type userTable struct {
	OID  uint32
	Name string
	View bool
	File uint32
}

// analyzedTables are the plain tables of userTables that have planner
// statistics.
const analyzedTables = userTables + ` AND c.relkind = 'r' AND c.reltuples <> 0`

func readUserTables(ctx context.Context, conn *pgx.Conn, analyzed bool) ([]userTable, error) {
	query := userTables
	if analyzed {
		query = analyzedTables
	}
	rows, err := conn.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("the user tables: %w", err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowToStructByPos[userTable])
	if err != nil {
		return nil, fmt.Errorf("the user tables: %w", err)
	}
	return tables, nil
}

const sequenceStates = `SELECT format('%I.%I', schemaname, sequencename), last_value, start_value FROM pg_sequences`

// readSnapshot reads the template's snapshot, under the shared lock: a
// template being rebuilt is not read.
func (db *DB) readSnapshot(ctx context.Context) (*snapshot, error) {
	admin, err := db.admin()
	if err != nil {
		return nil, err
	}
	var snap *snapshot
	err = db.withLock(ctx, admin, shared, func() error {
		s := &snapshot{}
		if err := admin.QueryRowContext(ctx, "SELECT oid FROM pg_database WHERE datname = $1", db.template).Scan(&s.template); err != nil {
			return fmt.Errorf("the oid of %s: %w", db.template, err)
		}
		conn, err := pgx.Connect(ctx, db.dsn(db.template))
		if err != nil {
			return fmt.Errorf("connect to %s: %w", db.template, err)
		}
		defer conn.Close(context.WithoutCancel(ctx))
		if err := conn.QueryRow(ctx, classesDigest).Scan(&s.classes); err != nil {
			return fmt.Errorf("the classes of %s: %w", db.template, err)
		}
		if s.filenodes, err = filenodes(ctx, conn); err != nil {
			return err
		}
		if s.sequences, err = sequences(ctx, conn); err != nil {
			return err
		}
		s.seeds, err = seeds(ctx, conn)
		snap = s
		return err
	})
	return snap, err
}

func filenodes(ctx context.Context, conn *pgx.Conn) (map[uint32]uint32, error) {
	tables, err := readUserTables(ctx, conn, false)
	if err != nil {
		return nil, err
	}
	files := make(map[uint32]uint32, len(tables))
	for _, tb := range tables {
		files[tb.OID] = tb.File
	}
	return files, nil
}

func sequences(ctx context.Context, conn *pgx.Conn) (map[string]sequenceState, error) {
	rows, err := conn.Query(ctx, sequenceStates)
	if err != nil {
		return nil, fmt.Errorf("the sequences: %w", err)
	}
	seqs := map[string]sequenceState{}
	var name string
	var s sequenceState
	_, err = pgx.ForEachRow(rows, []any{&name, &s.last, &s.start}, func() error {
		seqs[name] = s
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("the sequences: %w", err)
	}
	return seqs, nil
}

// seeds copies out the plain tables the planner does not know for empty.
func seeds(ctx context.Context, conn *pgx.Conn) (map[uint32][]byte, error) {
	tables, err := readUserTables(ctx, conn, true)
	if err != nil {
		return nil, err
	}
	out := map[uint32][]byte{}
	for _, tb := range tables {
		var buf bytes.Buffer
		if _, err := conn.PgConn().CopyTo(ctx, &buf, "COPY "+tb.Name+" TO STDOUT (FORMAT binary)"); err != nil {
			return nil, fmt.Errorf("copying out %s: %w", tb.Name, err)
		}
		out[tb.OID] = buf.Bytes()
	}
	return out, nil
}

// reuse puts the copy's data back as the template had it, or says why it
// cannot.
func (db *DB) reuse(ctx context.Context, c *pooled) error {
	if db.serverVersion < reuseSince {
		return fmt.Errorf("PostgreSQL %d has no pg_stat_force_next_flush", db.serverVersion)
	}
	db.snapOnce.Do(func() { db.snap, db.snapErr = db.readSnapshot(ctx) })
	if db.snapErr != nil {
		return db.snapErr
	}
	if c.template != db.snap.template {
		return errors.New("its template was rebuilt since")
	}
	if c.filenodes == nil {
		// A fresh copy has the template's files.
		c.filenodes = db.snap.filenodes
	}
	if c.keeper == nil {
		var err error
		if c.keeper, err = pgx.Connect(ctx, db.dsn(c.name)); err != nil {
			return fmt.Errorf("connect to reset: %w", err)
		}
	}
	conn := c.keeper
	if err := db.endSessions(ctx, c.name, conn.PgConn().PID()); err != nil {
		return err
	}
	changed, err := db.changedTables(ctx, conn, c)
	if err != nil {
		return err
	}
	seqs, err := db.changedSequences(ctx, conn)
	if err != nil {
		return err
	}
	if len(changed) == 0 && len(seqs) == 0 {
		return nil
	}
	if err := db.putBack(ctx, conn, changed, seqs); err != nil {
		return err
	}
	// The reset's own TRUNCATE, COPY and ANALYZE go to the statistics as the
	// backend goes idle after this call, and then are cleared with the
	// test's, so the next reset sees only the next test's changes.
	if _, err := conn.Exec(ctx, "SELECT pg_stat_force_next_flush()"); err != nil {
		return fmt.Errorf("flushing the reset's statistics: %w", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_stat_reset()"); err != nil {
		return fmt.Errorf("clearing the statistics: %w", err)
	}
	c.filenodes, err = filenodes(ctx, conn)
	return err
}

// endSessions ends the connections to the copy the test left, all but the
// keeper's, and waits until they are gone: a backend flushes its statistics
// as it exits, before it leaves pg_stat_activity.
func (db *DB) endSessions(ctx context.Context, name string, keeper uint32) error {
	admin, err := db.admin()
	if err != nil {
		return err
	}
	const others = "FROM pg_stat_activity WHERE datname = $1 AND pid <> $2"
	if _, err := admin.ExecContext(ctx, "SELECT pg_terminate_backend(pid) "+others, name, keeper); err != nil {
		return fmt.Errorf("ending the sessions of %s: %w", name, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		var left int
		if err := admin.QueryRowContext(ctx, "SELECT count(*) "+others, name, keeper).Scan(&left); err != nil {
			return fmt.Errorf("the sessions of %s: %w", name, err)
		}
		if left == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d sessions of %s outlived pg_terminate_backend by 10s", left, name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the sessions of %s: %w", name, ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}

// changedTables are the user tables the test changed. A changed system
// catalog other than pg_class, a pg_class that differs from the template's
// beyond the tables' files, or a changed materialized view make the copy
// unfit.
func (db *DB) changedTables(ctx context.Context, conn *pgx.Conn, c *pooled) ([]userTable, error) {
	rows, err := conn.Query(ctx, `SELECT s.relname FROM pg_stat_sys_tables s JOIN pg_class c ON c.oid = s.relid
		WHERE s.schemaname = 'pg_catalog' AND NOT c.relisshared AND s.n_tup_ins + s.n_tup_upd + s.n_tup_del > 0`)
	if err != nil {
		return nil, fmt.Errorf("the changed catalogs: %w", err)
	}
	catalogs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("the changed catalogs: %w", err)
	}
	classes := slices.Contains(catalogs, "pg_class")
	if others := slices.DeleteFunc(catalogs, func(s string) bool { return s == "pg_class" }); len(others) > 0 {
		return nil, fmt.Errorf("the test changed the catalogs %s", strings.Join(others, ", "))
	}
	if classes {
		var digest string
		if err := conn.QueryRow(ctx, classesDigest).Scan(&digest); err != nil {
			return nil, fmt.Errorf("the classes: %w", err)
		}
		if digest != db.snap.classes {
			return nil, errors.New("the test changed pg_class beyond the tables' files")
		}
	}
	tables, err := readUserTables(ctx, conn, false)
	if err != nil {
		return nil, err
	}
	rows, err = conn.Query(ctx, "SELECT relid FROM pg_stat_user_tables WHERE n_tup_ins + n_tup_upd + n_tup_del > 0")
	if err != nil {
		return nil, fmt.Errorf("the written tables: %w", err)
	}
	written, err := pgx.CollectRows(rows, pgx.RowTo[uint32])
	if err != nil {
		return nil, fmt.Errorf("the written tables: %w", err)
	}
	var changed []userTable
	for _, tb := range tables {
		if !slices.Contains(written, tb.OID) && tb.File == c.filenodes[tb.OID] {
			continue
		}
		if tb.View {
			return nil, fmt.Errorf("the test changed the materialized view %s", tb.Name)
		}
		changed = append(changed, tb)
	}
	return changed, nil
}

// changedSequences are the sequences whose state differs from the template's.
func (db *DB) changedSequences(ctx context.Context, conn *pgx.Conn) (map[string]sequenceState, error) {
	now, err := sequences(ctx, conn)
	if err != nil {
		return nil, err
	}
	for name, s := range now {
		if want, ok := db.snap.sequences[name]; ok && equalLast(s.last, want.last) {
			delete(now, name)
		}
	}
	for name := range now {
		if _, ok := db.snap.sequences[name]; !ok {
			return nil, fmt.Errorf("the test made the sequence %s", name)
		}
	}
	return now, nil
}

func equalLast(a, b *int64) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// putBack empties the changed tables and copies the template's rows back
// with the foreign keys and triggers off, sets the sequences back, and
// vacuums and analyzes the tables as the template was. DELETE, unlike
// TRUNCATE, needs no table referencing them emptied too, which costs a reset
// a new file and an ANALYZE for each; VACUUM takes the deleted rows away.
func (db *DB) putBack(ctx context.Context, conn *pgx.Conn, changed []userTable, seqs map[string]sequenceState) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin the reset: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica; SET LOCAL lock_timeout = '5s'"); err != nil {
		return fmt.Errorf("the reset's settings: %w", err)
	}
	names := make([]string, 0, len(changed))
	for _, tb := range changed {
		names = append(names, tb.Name)
		if _, err := tx.Exec(ctx, "DELETE FROM ONLY "+tb.Name); err != nil { //nolint:sql-injection // a regclass name, quoted by the catalog
			return fmt.Errorf("emptying %s: %w", tb.Name, err)
		}
		seed, ok := db.snap.seeds[tb.OID]
		if !ok {
			continue
		}
		if _, err := conn.PgConn().CopyFrom(ctx, bytes.NewReader(seed), "COPY "+tb.Name+" FROM STDIN (FORMAT binary)"); err != nil { //nolint:sql-injection // a regclass name, quoted by the catalog
			return fmt.Errorf("copying the rows of %s back: %w", tb.Name, err)
		}
	}
	for name, want := range db.snap.sequences {
		if _, ok := seqs[name]; !ok {
			continue
		}
		value, called := want.start, false
		if want.last != nil {
			value, called = *want.last, true
		}
		if _, err := tx.Exec(ctx, "SELECT setval($1::regclass, $2, $3)", name, value, called); err != nil {
			return fmt.Errorf("setting %s back: %w", name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit the reset: %w", err)
	}
	if len(names) > 0 {
		if _, err := conn.Exec(ctx, "VACUUM (ANALYZE) "+strings.Join(names, ", ")); err != nil {
			return fmt.Errorf("vacuuming the reset tables: %w", err)
		}
	}
	return nil
}

// take hands out a pooled copy, if one is free.
func (db *DB) take() *pooled {
	db.poolMu.Lock()
	defer db.poolMu.Unlock()
	if len(db.free) == 0 {
		return nil
	}
	c := db.free[len(db.free)-1]
	db.free = db.free[:len(db.free)-1]
	return c
}

func (db *DB) put(c *pooled) {
	db.poolMu.Lock()
	defer db.poolMu.Unlock()
	db.free = append(db.free, c)
}

// ownerBits is the size of the owner key in a copy's name: 12 hex digits.
const ownerBits = 48

// ownerRe finds the owner key in a copy's name: <owner>p<random>. Names
// before the pool had 24 hex digits, no p, and are left alone.
var ownerRe = regexp.MustCompile(`_([0-9a-f]{12})p[0-9a-f]{11}$`)

// own takes the binary's owner key, an advisory lock its connection holds
// while the binary lives, and drops the copies of the binaries that are gone:
// their pools outlive them.
func (db *DB) own(ctx context.Context) error {
	db.ownerOnce.Do(func() { db.ownerErr = db.takeOwnerKey(ctx) })
	return db.ownerErr
}

func (db *DB) takeOwnerKey(ctx context.Context) error {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Errorf("an owner key: %w", err)
	}
	key := int64(binary.BigEndian.Uint64(b[:]) >> (64 - ownerBits))
	admin, err := db.admin()
	if err != nil {
		return err
	}
	// The connection stays out of the pool as long as the binary lives: its
	// end releases the key.
	conn, err := admin.Conn(context.WithoutCancel(ctx))
	if err != nil {
		return fmt.Errorf("a connection for the owner key: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", key); err != nil {
		return errors.Join(fmt.Errorf("taking the owner key: %w", err), conn.Close())
	}
	db.owner = key
	return db.dropOrphans(ctx, admin)
}

// dropOrphans drops the copies whose owner key nobody holds.
func (db *DB) dropOrphans(ctx context.Context, admin *sql.DB) error {
	rows, err := admin.QueryContext(ctx, "SELECT datname FROM pg_database WHERE starts_with(datname, $1)", db.prefix+"_")
	if err != nil {
		return fmt.Errorf("the copies: %w", err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return fmt.Errorf("the copies: %w", err)
		}
		names = append(names, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return fmt.Errorf("the copies: %w", err)
	}
	held, err := heldKeys(ctx, admin)
	if err != nil {
		return err
	}
	var orphans []string
	for _, name := range names {
		m := ownerRe.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		key, err := strconv.ParseInt(m[1], 16, 64)
		if err != nil {
			return fmt.Errorf("the owner of %s: %w", name, err)
		}
		if !held[key] {
			orphans = append(orphans, name)
		}
	}
	for _, name := range orphans {
		if err := db.drop(ctx, name); err != nil { //nolint:query-in-loop // DROP DATABASE takes one database
			return fmt.Errorf("dropping the orphan %s: %w", name, err)
		}
	}
	return nil
}

// heldKeys are the advisory locks with a bigint key held in the postgres
// database: the owner keys of the binaries alive. The copies are listed
// before: a key taken after that has no copies among them.
func heldKeys(ctx context.Context, admin *sql.DB) (map[int64]bool, error) {
	rows, err := admin.QueryContext(ctx, `SELECT (classid::bigint << 32) | objid::bigint FROM pg_locks
		WHERE locktype = 'advisory' AND objsubid = 1 AND granted
		AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	if err != nil {
		return nil, fmt.Errorf("the owner keys held: %w", err)
	}
	held := map[int64]bool{}
	for rows.Next() {
		var key int64
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return nil, fmt.Errorf("the owner keys held: %w", err)
		}
		held[key] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("the owner keys held: %w", err)
	}
	return held, nil
}

// admin is the binary's pool of connections to the server's postgres
// database, open while the binary lives.
func (db *DB) admin() (*sql.DB, error) {
	db.adminOnce.Do(func() {
		db.adminDB, db.adminErr = sql.Open("pgx", db.dsn("postgres"))
		if db.adminErr != nil {
			db.adminErr = fmt.Errorf("connect to the postgres database: %w", db.adminErr)
		}
	})
	return db.adminDB, db.adminErr
}
