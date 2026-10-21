package testdb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/aiseeq/graft/v2/internal/config"
)

// staleAfter is how long a source of another migration set may go unused
// before it is dropped with the databases made from it.
const staleAfter = 7 * 24 * time.Hour

// usedMark starts the comment a source carries: when graft last brought it
// up.
const usedMark = "graft used "

// SourceFor is the source database of the work tree at root: the configured
// database, or with test_db.migrations the one of their hash.
func SourceFor(cfg *config.TestDB, root string) (string, error) {
	if cfg.Migrations == "" {
		return cfg.Database, nil
	}
	h := sha256.New()
	dir := filepath.Join(root, filepath.FromSlash(cfg.Migrations))
	err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || !e.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(rel), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("test_db.migrations: %w", err)
	}
	return cfg.SourceName(hex.EncodeToString(h.Sum(nil))[:config.MigrationHashLen]), nil
}

// prepareSource creates the source of a migration set if it is missing,
// marks it used, and drops the sources of other sets unused for staleAfter
// with every database made from them.
func (d *DB) prepareSource(ctx context.Context, source string) error {
	if source == d.Cfg.Database {
		return nil
	}
	conn, err := d.connect(ctx)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	rows, err := query(ctx, conn, "SELECT datname, coalesce(shobj_description(oid, 'pg_database'), '') FROM pg_database")
	if err != nil {
		return err
	}
	comments := map[string]string{}
	for _, row := range rows {
		comments[row[0]] = row[1]
	}
	if _, ok := comments[source]; !ok {
		fmt.Fprintf(d.Log, "graft: creating %s for this set of migrations\n", source)
		if _, err := query(ctx, conn, "CREATE DATABASE "+source); err != nil {
			return err
		}
	}
	if _, err := query(ctx, conn, fmt.Sprintf("COMMENT ON DATABASE %s IS '%s%s'", source, usedMark, time.Now().UTC().Format(time.RFC3339))); err != nil {
		return err
	}
	return d.dropStale(ctx, conn, source, comments)
}

// dropStale drops the sources of other migration sets that were not used for
// staleAfter, and the databases made from them (the template and copies the
// testdb package names after the source).
func (d *DB) dropStale(ctx context.Context, conn *pgx.Conn, source string, comments map[string]string) error {
	hash := "[0-9a-f]{" + fmt.Sprint(config.MigrationHashLen) + "}"
	stem, derived := strings.CutSuffix(d.Cfg.Database, config.SourceSuffix)
	// family matches a database of a migration set; its first group names
	// the set's source.
	family := regexp.MustCompile("^(" + regexp.QuoteMeta(d.Cfg.Database) + "_" + hash + ")$")
	if derived {
		family = regexp.MustCompile("^(" + regexp.QuoteMeta(stem) + "_" + hash + ")_")
	}
	sets := map[string][]string{}
	for name := range comments {
		if m := family.FindStringSubmatch(name); m != nil {
			sets[m[1]] = append(sets[m[1]], name)
		}
	}
	for set, names := range sets {
		src := set
		if derived {
			src += config.SourceSuffix
		}
		if src == source {
			continue
		}
		used, err := time.Parse(time.RFC3339, strings.TrimPrefix(comments[src], usedMark))
		if err == nil && time.Since(used) < staleAfter {
			continue
		}
		for _, db := range names {
			fmt.Fprintf(d.Log, "graft: dropping %s: its migration set was not used for %s\n", db, staleAfter)
			if _, err := query(ctx, conn, "DROP DATABASE IF EXISTS "+db+" WITH (FORCE)"); err != nil {
				return err
			}
		}
	}
	return nil
}
