// Package testdb gives every test its own PostgreSQL database, a copy of a
// migrated source database made through a template, so tests and test
// binaries run in parallel without seeing each other's rows.
//
// TEST_DB_DSN names the source database: a disposable instance, the database
// name ending in _source and containing "test", for example
// postgres://u:p@127.0.0.1:5432/shop_test_source. The template is then
// shop_test_template and the copies shop_test_<test>_<random>. The template is
// rebuilt when the version in the source's schema_migrations changes.
package testdb

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aiseeq/graft/v2/internal/pgsql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // the pgx driver for database/sql
)

// DSNEnv names the source database.
const DSNEnv = "TEST_DB_DSN"

const (
	sourceSuffix   = "_source"
	templateSuffix = "_template"
	// maxIdentifier is PostgreSQL's identifier length.
	maxIdentifier = 63
	// suffixLen is the length of the end of a copy's name that makes it
	// unique: the owner key, p, and random hex digits.
	suffixLen = 24
	// maxTestPart caps the part of a copy's name taken from the test's.
	maxTestPart = 16
)

// DB is a source database and the names derived from it.
type DB struct {
	base     *url.URL
	source   string
	template string
	prefix   string
	lockID   int64

	once        sync.Once
	templateErr error
	// serverVersion is the server's server_version_num, read with the
	// template.
	serverVersion int

	// adminDB is the binary's connections to the server's postgres database.
	adminOnce sync.Once
	adminDB   *sql.DB
	adminErr  error

	// The pool of copies the binary's tests are done with.
	poolMu sync.Mutex
	free   []*pooled

	snapOnce sync.Once
	snap     *snapshot
	snapErr  error

	ownerOnce sync.Once
	ownerErr  error
	// owner is the binary's key in the names of its copies, an advisory lock
	// a connection of the admin pool holds.
	owner int64
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*DB{}
)

// fromEnv is the DB of TEST_DB_DSN, one per DSN in the process, so the
// template is checked once per test binary.
func fromEnv() (*DB, error) {
	dsn := os.Getenv(DSNEnv)
	if dsn == "" {
		return nil, fmt.Errorf("%s is not set: it names the migrated source database of a disposable PostgreSQL", DSNEnv)
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if db, ok := cache[dsn]; ok {
		return db, nil
	}
	db, err := newDB(dsn)
	if err != nil {
		return nil, err
	}
	cache[dsn] = db
	return db, nil
}

// newDB checks the source DSN and derives the template and copy names. A
// database that does not look like a test one is refused before anything is
// created or dropped.
func newDB(dsn string) (*DB, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", DSNEnv, err)
	}
	name := strings.TrimPrefix(u.Path, "/")
	if !strings.Contains(strings.ToLower(name), "test") {
		return nil, fmt.Errorf("%s: database %q has no \"test\" in its name; copies are made and dropped next to it, so it must be a disposable test database", DSNEnv, name)
	}
	prefix, ok := strings.CutSuffix(name, sourceSuffix)
	if !ok || prefix == "" {
		return nil, fmt.Errorf("%s: database %q must be named <prefix>%s, the template and the copies take the prefix", DSNEnv, name, sourceSuffix)
	}
	if room := maxIdentifier - len(prefix) - 2 - suffixLen; room < 1 {
		return nil, fmt.Errorf("%s: prefix %q is too long for copy names within %d characters", DSNEnv, prefix, maxIdentifier)
	}
	h := fnv.New64a()
	h.Write([]byte(name))
	return &DB{
		base:     u,
		source:   name,
		template: prefix + templateSuffix,
		prefix:   prefix,
		lockID:   int64(h.Sum64()), // an advisory lock key: any 64 bits do
	}, nil
}

// dsn is the DSN of database name on the source's server.
func (db *DB) dsn(name string) string {
	u := *db.base
	u.Path = "/" + name
	return u.String()
}

// NewDSN makes a copy of the source for the test and returns its DSN. The
// copy is dropped when the test ends.
func NewDSN(t *testing.T) string {
	t.Helper()
	db, err := fromEnv()
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	return db.NewDSN(t)
}

// NewDSN gives the test a copy of the source, one another test of the binary
// is done with and whose data was put back, or a new one, and returns its
// DSN.
func (db *DB) NewDSN(t *testing.T) string {
	t.Helper()
	db.ensureTemplate(t)
	c := db.take()
	if c == nil {
		var err error
		if c, err = db.create(t.Context(), t.Name()); err != nil {
			t.Fatalf("testdb: %v", err)
		}
	}
	t.Cleanup(func() {
		// The test's context is done by now; the reset or the drop must
		// still run.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), time.Minute)
		defer cancel()
		err := db.reuse(ctx, c)
		if err == nil {
			db.put(c)
			return
		}
		t.Logf("testdb: dropping %s, not reused: %v", c.name, err)
		if c.keeper != nil {
			_ = c.keeper.Close(ctx) // the drop ends the session anyway
		}
		if err := db.drop(ctx, c.name); err != nil {
			t.Errorf("testdb: dropping %s: %v", c.name, err)
		}
	})
	return db.dsn(c.name)
}

// create makes a new copy of the template.
func (db *DB) create(ctx context.Context, test string) (*pooled, error) {
	if err := db.own(ctx); err != nil {
		return nil, err
	}
	name, err := db.copyName(test)
	if err != nil {
		return nil, err
	}
	admin, err := db.admin()
	if err != nil {
		return nil, err
	}
	c := &pooled{name: name}
	// Shared: copies are made side by side, never while another binary holds
	// the exclusive lock to rebuild the template. The template's connections
	// are left alone: since PostgreSQL 15 a template with readers copies fine,
	// and terminating them kills the parallel binaries reading its version.
	err = db.withLock(ctx, admin, shared, func() error {
		if _, err := admin.ExecContext(ctx, createStatement(name, db.template, db.serverVersion)); err != nil {
			return err
		}
		return admin.QueryRowContext(ctx, "SELECT oid FROM pg_database WHERE datname = $1", db.template).Scan(&c.template)
	})
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", name, err)
	}
	return c, nil
}

var nonAlphaNum = regexp.MustCompile(`[^a-z0-9]+`)

// copyName is <prefix>_<test>_<owner>p<random>, within PostgreSQL's
// identifier length: the owner key tells whose pool the copy is in.
func (db *DB) copyName(test string) (string, error) {
	part := nonAlphaNum.ReplaceAllString(strings.ToLower(test), "_")
	part = part[:min(len(part), maxTestPart, maxIdentifier-len(db.prefix)-2-suffixLen)]
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("a random name: %w", err)
	}
	return fmt.Sprintf("%s_%s_%012xp%s", db.prefix, part, db.owner, hex.EncodeToString(b)[:11]), nil
}

func (db *DB) drop(ctx context.Context, name string) error {
	admin, err := db.admin()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+ident(name)+" WITH (FORCE)")
	return err
}

// ensureTemplate rebuilds the template once per binary if it is missing or
// its migration version differs from the source's.
func (db *DB) ensureTemplate(t *testing.T) {
	t.Helper()
	db.once.Do(func() {
		db.templateErr = db.refreshTemplate(t.Context())
	})
	if db.templateErr != nil {
		t.Fatalf("testdb: template %s: %v", db.template, db.templateErr)
	}
}

func (db *DB) refreshTemplate(ctx context.Context) error {
	admin, err := sql.Open("pgx", db.dsn("postgres"))
	if err != nil {
		return fmt.Errorf("connect to refresh the template: %w", err)
	}
	defer admin.Close()
	if err := admin.QueryRowContext(ctx, "SELECT current_setting('server_version_num')::int").Scan(&db.serverVersion); err != nil {
		return fmt.Errorf("reading the server version: %w", err)
	}
	// Exclusive: one binary rebuilds the template, and nobody copies it
	// meanwhile.
	return db.withLock(ctx, admin, exclusive, func() error {
		fresh, err := db.templateFresh(ctx, admin)
		if err != nil || fresh {
			return err
		}
		if _, err := admin.ExecContext(ctx, "DROP DATABASE IF EXISTS "+ident(db.template)+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("dropping the stale template: %w", err)
		}
		if err := db.analyzeSource(ctx); err != nil {
			return err
		}
		if _, err := admin.ExecContext(ctx, createStatement(db.template, db.source, db.serverVersion)); err != nil {
			return fmt.Errorf("copying %s: %w", db.source, err)
		}
		return nil
	})
}

// analyzeSource gathers the planner statistics of the source, which the
// template and every copy inherit: rows a migration loads have none until
// autovacuum comes, and a planner that takes a table for empty picks plans
// thousands of times slower. The connection is closed before the copy, which
// a database with connections refuses.
func (db *DB) analyzeSource(ctx context.Context) error {
	conn, err := sql.Open("pgx", db.dsn(db.source))
	if err != nil {
		return fmt.Errorf("connect to analyze %s: %w", db.source, err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "ANALYZE"); err != nil {
		return fmt.Errorf("analyzing %s: %w", db.source, err)
	}
	return nil
}

// fileCopySince is the server_version_num of PostgreSQL 15, which added
// CREATE DATABASE ... STRATEGY.
const fileCopySince = 150000

// createStatement makes database name from template. FILE_COPY copies the
// files after a checkpoint instead of writing the whole template to the WAL,
// which costs less CPU when tests write in parallel; older servers copy the
// files anyway.
func createStatement(name, template string, serverVersion int) string {
	stmt := "CREATE DATABASE " + ident(name) + " TEMPLATE " + ident(template)
	if serverVersion >= fileCopySince {
		stmt += " STRATEGY FILE_COPY"
	}
	return stmt
}

// templateFresh tells whether the template exists with the source's
// migration version.
func (db *DB) templateFresh(ctx context.Context, admin *sql.DB) (bool, error) {
	var exists bool
	if err := admin.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", db.template).Scan(&exists); err != nil {
		return false, fmt.Errorf("look up the template %s: %w", db.template, err)
	}
	if !exists {
		return false, nil
	}
	source, err := db.migrationVersion(ctx, db.dsn(db.source))
	if err != nil {
		return false, fmt.Errorf("%s: %w", db.source, err)
	}
	template, err := db.migrationVersion(ctx, db.dsn(db.template))
	if err != nil {
		return false, fmt.Errorf("%s: %w", db.template, err)
	}
	return source == template, nil
}

// migrationVersion is the version in the database's schema_migrations.
func (db *DB) migrationVersion(ctx context.Context, dsn string) (int64, error) {
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		return 0, fmt.Errorf("connect to read the migration version: %w", err)
	}
	defer conn.Close()
	var v int64
	if err := conn.QueryRowContext(ctx, "SELECT version FROM schema_migrations LIMIT 1").Scan(&v); err != nil {
		return 0, fmt.Errorf("reading schema_migrations: %w", err)
	}
	return v, nil
}

type lockMode struct{ acquire, release string }

var (
	exclusive = lockMode{"SELECT pg_advisory_lock($1)", "SELECT pg_advisory_unlock($1)"}
	shared    = lockMode{"SELECT pg_advisory_lock_shared($1)", "SELECT pg_advisory_unlock_shared($1)"}
)

// withLock runs fn under the source's advisory lock. The lock lives on one
// connection, so a binary that dies releases it.
func (db *DB) withLock(ctx context.Context, admin *sql.DB, mode lockMode, fn func() error) (err error) {
	conn, err := admin.Conn(ctx)
	if err != nil {
		return fmt.Errorf("a connection for the lock: %w", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, mode.acquire, db.lockID); err != nil {
		return fmt.Errorf("taking the template lock: %w", err)
	}
	defer func() {
		if _, uerr := conn.ExecContext(context.WithoutCancel(ctx), mode.release, db.lockID); uerr != nil {
			err = errors.Join(err, fmt.Errorf("releasing the template lock: %w", uerr))
		}
	}()
	return fn()
}

func ident(name string) string {
	return pgx.Identifier{name}.Sanitize()
}

// Connect makes a copy of the source for the test and returns a pinged pool
// on it, closed when the test ends.
func Connect(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(NewDSN(t))
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	cfg.MaxConns = 5
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 5 * time.Minute
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("testdb: ping: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Truncate empties the tables, plain or schema-qualified, as TRUNCATE ...
// CASCADE would: every table that references them, directly or through
// others, is emptied too. Tables already empty are left alone, a TRUNCATE
// costing new files for each table; it all runs as one statement, the tables
// locked first as TRUNCATE locks them.
func Truncate(t *testing.T, pool *pgxpool.Pool, tables ...string) {
	t.Helper()
	if len(tables) == 0 {
		t.Fatal("testdb: Truncate without tables")
	}
	names := make([]string, len(tables))
	for i, name := range tables {
		names[i] = pgsql.Literal(pgx.Identifier(strings.Split(name, ".")).Sanitize()) + "::regclass"
	}
	if _, err := pool.Exec(t.Context(), truncateBlock(names)); err != nil {
		t.Fatalf("testdb: truncate: %v", err)
	}
}

// truncateBlock truncates the non-empty tables of the closure of tables (as
// regclass expressions) under the foreign keys that reference them.
func truncateBlock(tables []string) string {
	body := `
DECLARE
	closure regclass[];
	nonempty text[] := '{}';
	r regclass;
	found boolean;
BEGIN
	WITH RECURSIVE c(rel) AS (
		SELECT unnest(ARRAY[` + strings.Join(tables, ", ") + `])
		UNION
		SELECT k.conrelid::regclass FROM pg_constraint k JOIN c ON k.confrelid = c.rel WHERE k.contype = 'f'
	)
	SELECT array_agg(rel) INTO closure FROM c;
	EXECUTE 'LOCK TABLE ' || array_to_string(closure::text[], ', ') || ' IN ACCESS EXCLUSIVE MODE';
	FOREACH r IN ARRAY closure LOOP
		EXECUTE format('SELECT EXISTS (SELECT FROM %s)', r) INTO found;
		IF found THEN
			nonempty := nonempty || r::text;
		END IF;
	END LOOP;
	IF cardinality(nonempty) > 0 THEN
		EXECUTE 'TRUNCATE ' || array_to_string(nonempty, ', ') || ' CASCADE';
	END IF;
END`
	return "DO " + pgsql.Literal(body)
}
