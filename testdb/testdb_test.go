package testdb

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
)

// source prepares a source database with a migration version, the way a
// project's migrate step leaves it, and points TEST_DB_DSN at it. graft test
// starts PostgreSQL and exports GRAFT_TEST_PG_DSN.
func source(t *testing.T, version int) {
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
	for _, q := range []string{
		"CREATE TABLE schema_migrations (version bigint NOT NULL, dirty boolean NOT NULL)",
		"CREATE TABLE items (id int)",
		"INSERT INTO items VALUES (1)",
	} {
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
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := db.migrationVersion(context.Background(), db.dsn(db.template)); err != nil {
					errCh <- err
					return
				}
			}
		})
	}
	for range 20 {
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
	name, err := db.copyName(strings.Repeat("TestSomethingLong/", 5))
	if err != nil || len(name) > 63 || !strings.HasPrefix(name, db.prefix+"_") {
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
