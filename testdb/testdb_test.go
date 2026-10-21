package testdb

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// source prepares a source database with a migration version and the extra
// statements, the way a project's migrate step leaves it, and points
// TEST_DB_DSN at it. graft test starts PostgreSQL and exports
// GRAFT_TEST_PG_DSN.
func source(t *testing.T, version int, extra ...string) {
	t.Helper()
	admin := os.Getenv("GRAFT_TEST_PG_DSN")
	if admin == "" {
		t.Skip("GRAFT_TEST_PG_DSN is not set: run graft test")
	}
	db, err := sql.Open("pgx", admin)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const name = "graft_testdb_test_source"
	if _, err := db.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	src, err := sql.Open("pgx", u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	for _, q := range append([]string{
		"CREATE TABLE schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)",
		"CREATE TABLE items (id int)",
		"INSERT INTO items VALUES (1)",
	}, extra...) {
		if _, err := src.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := src.Exec("INSERT INTO schema_migrations VALUES ($1, false)", version); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DSNEnv, u.String())
}

// TestConnectGivesAnIsolatedCopy: every test gets its own copy of the source
// database through a template, dropped afterwards; a new migration version
// rebuilds the template.
func TestConnectGivesAnIsolatedCopy(t *testing.T) {
	source(t, 1)
	a, b := Connect(t), Connect(t)
	if _, err := a.Exec(t.Context(), "INSERT INTO items VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.QueryRow(t.Context(), "SELECT count(*) FROM items").Scan(&n); err != nil || n != 1 {
		t.Errorf("the second copy sees %d items, %v", n, err)
	}
	Truncate(t, a, "items")
	if err := a.QueryRow(t.Context(), "SELECT count(*) FROM items").Scan(&n); err != nil || n != 0 {
		t.Errorf("after Truncate: %d items, %v", n, err)
	}
}

// TestRefusesANonTestDatabase: a DSN whose database is not a test one is
// refused before anything is created or dropped.
func TestRefusesANonTestDatabase(t *testing.T) {
	if _, err := newDB("postgres://u:p@127.0.0.1:5432/shop"); err == nil {
		t.Error("a production-looking database was accepted")
	}
	db, err := newDB("postgres://u:p@127.0.0.1:5432/shop_test_source")
	if err != nil || db.template != "shop_test_template" || db.prefix != "shop_test" {
		t.Errorf("names: %+v, %v", db, err)
	}
}

// TestCloningLeavesTemplateReadersAlone: handing out a copy must not
// terminate the connections that read the template, as parallel test binaries
// do on start to check the template is fresh; terminating them used to kill
// whole packages run side by side.
func TestCloningLeavesTemplateReadersAlone(t *testing.T) {
	source(t, 1)
	db, err := fromEnv()
	if err != nil {
		t.Fatal(err)
	}
	db.ensureTemplate(t)
	stop := make(chan struct{})
	errCh := make(chan error, 8)
	read := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for first := true; ; first = false {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := db.migrationVersion(context.Background(), db.dsn(db.template)); err != nil {
					errCh <- err
					return
				}
				if first {
					read <- struct{}{}
				}
			}
		})
	}
	// Every reader is at work before the copies: a copy that terminated
	// them fails the test from the first one on.
	for range 8 {
		select {
		case <-read:
		case err := <-errCh:
			t.Fatalf("a template reader: %v", err)
		}
	}
	for range 3 {
		db.NewDSN(t)
	}
	close(stop)
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		t.Fatalf("a template reader: %v", err)
	}
}

// TestTemplateFollowsTheMigrationVersion: a binary that finds the source at a
// new migration version rebuilds the template from it.
func TestTemplateFollowsTheMigrationVersion(t *testing.T) {
	source(t, 1)
	first, err := newDB(os.Getenv(DSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	first.ensureTemplate(t)
	src, err := sql.Open("pgx", first.dsn(first.source))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Exec("UPDATE schema_migrations SET version = 2"); err != nil {
		t.Fatal(err)
	}
	src.Close()
	second, err := newDB(os.Getenv(DSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	second.ensureTemplate(t)
	if v, err := second.migrationVersion(t.Context(), second.dsn(second.template)); err != nil || v != 2 {
		t.Errorf("template version %d, %v", v, err)
	}
}

// TestCopyNamesFitPostgreSQL: a long test name is cut so the copy's name stays
// within 63 characters.
func TestCopyNamesFitPostgreSQL(t *testing.T) {
	db, err := newDB("postgres://u:p@127.0.0.1:5432/" + strings.Repeat("x", 30) + "_test_source")
	if err != nil {
		t.Fatal(err)
	}
	db.owner = 1<<48 - 1
	name, err := db.copyName(strings.Repeat("TestSomethingLong/", 5))
	if err != nil || len(name) > 63 || !strings.HasPrefix(name, db.prefix+"_") || !ownerRe.MatchString(name) {
		t.Errorf("copy name %q (%d), %v", name, len(name), err)
	}
	if _, err := newDB("postgres://u:p@127.0.0.1:5432/" + strings.Repeat("x", 40) + "_test_source"); err == nil {
		t.Error("a prefix leaving no room for copy names was accepted")
	}
}

// TestCopiesCarryTheSourceStatistics: rows a migration loads leave the source
// without planner statistics until autovacuum comes; the template is made
// from an analyzed source, so every copy plans with the real row counts.
func TestCopiesCarryTheSourceStatistics(t *testing.T) {
	source(t, 7)
	db, err := newDB(os.Getenv(DSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	src, err := sql.Open("pgx", db.dsn(db.source))
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Exec("INSERT INTO items SELECT g FROM generate_series(2, 5000) g")
	src.Close() // a database with connections cannot be copied
	if err != nil {
		t.Fatal(err)
	}
	copyDB, err := sql.Open("pgx", db.NewDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.Close()
	var tuples float64
	if err := copyDB.QueryRow("SELECT reltuples FROM pg_class WHERE relname = 'items'").Scan(&tuples); err != nil {
		t.Fatal(err)
	}
	if tuples != 5000 {
		t.Errorf("the copy's planner sees %v rows in items, want 5000", tuples)
	}
}

// TestCreateStatement: copies are made with STRATEGY FILE_COPY where the
// server has it (PostgreSQL 15 and later): no WAL of the whole template per
// copy.
func TestCreateStatement(t *testing.T) {
	for _, c := range []struct {
		version int
		want    string
	}{
		{180004, `CREATE DATABASE "a_x" TEMPLATE "a_template" STRATEGY FILE_COPY`},
		{150000, `CREATE DATABASE "a_x" TEMPLATE "a_template" STRATEGY FILE_COPY`},
		{140011, `CREATE DATABASE "a_x" TEMPLATE "a_template"`},
	} {
		if got := createStatement("a_x", "a_template", c.version); got != c.want {
			t.Errorf("server %d: %s, want %s", c.version, got, c.want)
		}
	}
}

// TestTruncateSkipsEmptyTables: Truncate leaves the empty tables alone, a
// TRUNCATE costing a new file per table, and still empties every table the
// CASCADE would have reached.
func TestTruncateSkipsEmptyTables(t *testing.T) {
	source(t, 1)
	pool := Connect(t)
	ctx := t.Context()
	for _, q := range []string{
		"DELETE FROM items",
		"CREATE TABLE empty_one (id int)",
		"CREATE TABLE parent (id int PRIMARY KEY)",
		"CREATE TABLE child (parent_id int REFERENCES parent)",
		"CREATE TABLE grandchild (child_ref int, other_parent int REFERENCES parent)",
		"INSERT INTO child VALUES (NULL)",
		"INSERT INTO grandchild VALUES (1, NULL)",
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	filenode := func(table string) uint32 {
		t.Helper()
		var n uint32
		if err := pool.QueryRow(ctx, "SELECT pg_relation_filenode($1::regclass)", table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := filenode("empty_one")
	Truncate(t, pool, "empty_one")
	if filenode("empty_one") != before {
		t.Error("an empty table was truncated")
	}
	parent := filenode("parent")
	Truncate(t, pool, "parent", "public.empty_one")
	var left int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM child) + (SELECT count(*) FROM grandchild)").Scan(&left); err != nil || left != 0 {
		t.Errorf("tables the cascade reaches keep %d rows, %v", left, err)
	}
	if filenode("parent") != parent {
		t.Error("the empty parent was truncated")
	}
}

// richSource is a source with rows in tables a foreign key ties, a serial, a
// loose sequence and an empty table.
func richSource(t *testing.T) *DB {
	t.Helper()
	source(t, 3,
		"CREATE TABLE parent (id serial PRIMARY KEY, name text)",
		"INSERT INTO parent (name) VALUES ('a'), ('b')",
		"CREATE TABLE child (parent_id int REFERENCES parent, note text)",
		"INSERT INTO child VALUES (1, 'x')",
		"CREATE TABLE empty_one (id int)",
		"CREATE SEQUENCE loose",
	)
	db, err := newDB(os.Getenv(DSNEnv))
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// state is what a test of the copy can see of richSource's data: rows,
// sequences and the planner's row counts.
func state(t *testing.T, dsn string) string {
	t.Helper()
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var parent, child, seqs, tuples string
	var items, empty int
	err = conn.QueryRow(`SELECT
		(SELECT string_agg(id || ':' || name, ',' ORDER BY id) FROM parent),
		(SELECT string_agg(parent_id || ':' || note, ',') FROM child),
		(SELECT count(*) FROM items), (SELECT count(*) FROM empty_one),
		(SELECT format('%s/%s %s/%s', p.last_value, p.is_called, l.last_value, l.is_called) FROM parent_id_seq p, loose l),
		(SELECT string_agg(relname || '=' || reltuples, ',' ORDER BY relname) FROM pg_class WHERE relname IN ('parent', 'child', 'items', 'empty_one'))`,
	).Scan(&parent, &child, &items, &empty, &seqs, &tuples)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("parent %s; child %s; items %d; empty_one %d; sequences %s; reltuples %s", parent, child, items, empty, seqs, tuples)
}

// TestReusedCopyIsLikeAFreshOne: a copy a test is done with goes to the next
// test of the binary with its rows, sequences and planner statistics put
// back as a fresh copy has them, whatever the test wrote, emptied or left
// connected.
func TestReusedCopyIsLikeAFreshOne(t *testing.T) {
	db := richSource(t)
	db.ensureTemplate(t)
	var first, fresh string
	t.Run("dirty", func(t *testing.T) {
		// A connection the test leaves open: closed after the reset.
		var left *sql.DB
		t.Cleanup(func() { left.Close() })
		first = db.NewDSN(t)
		fresh = state(t, first)
		var err error
		if left, err = sql.Open("pgx", first); err != nil {
			t.Fatal(err)
		}
		for _, q := range []string{
			"INSERT INTO parent (name) VALUES ('c')",
			"UPDATE child SET note = 'y'",
			"TRUNCATE items",
			"SELECT nextval('loose')",
			"INSERT INTO empty_one VALUES (1)",
		} {
			if _, err := left.Exec(q); err != nil {
				t.Fatal(err)
			}
		}
	})
	if strings.Contains(fresh, "parent ;") || !strings.Contains(fresh, "items 1") {
		t.Fatalf("the fresh copy: %s", fresh)
	}
	t.Run("next", func(t *testing.T) {
		dsn := db.NewDSN(t)
		if dsn != first {
			t.Errorf("a new copy %s instead of the reused %s", dsn, first)
		}
		if got := state(t, dsn); got != fresh {
			t.Errorf("the reused copy:\n%s\nwant as fresh:\n%s", got, fresh)
		}
	})
	t.Run("clean", func(t *testing.T) {
		// A test that changes nothing leaves the copy as it is.
		if dsn := db.NewDSN(t); dsn != first {
			t.Errorf("a new copy %s instead of the reused %s", dsn, first)
		}
	})
}

// TestSchemaChangeDropsTheCopy: a copy whose schema or planner statistics a
// test changed is dropped, as every copy was before the pool, and the next
// test gets a fresh one.
func TestSchemaChangeDropsTheCopy(t *testing.T) {
	db := richSource(t)
	db.ensureTemplate(t)
	for _, change := range []string{
		"CREATE TABLE extra (id int)",
		"ALTER TABLE child ALTER COLUMN note SET DEFAULT 'n'",
		"CREATE INDEX ON child (note)",
		"ANALYZE parent",
		"CREATE SEQUENCE another",
	} {
		var name string
		t.Run(change, func(t *testing.T) {
			dsn := db.NewDSN(t)
			name = dbName(t, dsn)
			conn, err := sql.Open("pgx", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// ANALYZE writes no statistics a fresh copy lacks unless the
			// data differs.
			if _, err := conn.Exec("INSERT INTO parent (name) SELECT 'n' FROM generate_series(1, 100)"); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(change); err != nil {
				t.Fatal(err)
			}
		})
		if exists(t, db, name) {
			t.Errorf("%s: the copy %s was kept", change, name)
		}
	}
}

func dbName(t *testing.T, dsn string) string {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(u.Path, "/")
}

func exists(t *testing.T, db *DB, name string) bool {
	t.Helper()
	admin, err := sql.Open("pgx", db.dsn("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	var ok bool
	if err := admin.QueryRow("SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestOrphanCopiesAreDropped: the pooled copies of a binary that is gone,
// whose owner key nobody holds, are dropped when another binary makes its
// first copy; those of a live binary, and copies named before the pool, stay.
func TestOrphanCopiesAreDropped(t *testing.T) {
	db := richSource(t)
	db.ensureTemplate(t)
	admin, err := sql.Open("pgx", db.dsn("postgres"))
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	live, err := admin.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	const liveKey, deadKey = 0xabc, 0xdef
	if _, err := live.ExecContext(t.Context(), "SELECT pg_advisory_lock($1)", liveKey); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{
		fmt.Sprintf("%s_x_%012xp%011x", db.prefix, deadKey, 1): false,
		fmt.Sprintf("%s_x_%012xp%011x", db.prefix, liveKey, 2): true,
		db.prefix + "_x_" + strings.Repeat("0123456789ab", 2):  true,
	}
	for name := range names {
		if _, err := admin.Exec("CREATE DATABASE " + ident(name)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.drop(context.WithoutCancel(t.Context()), name); err != nil {
				t.Error(err)
			}
		})
	}
	db.NewDSN(t)
	for name, kept := range names {
		if exists(t, db, name) != kept {
			t.Errorf("%s: kept %v, want %v", name, !kept, kept)
		}
	}
}
