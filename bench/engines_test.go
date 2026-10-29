// Package bench compares cheesedb with SQLite and PostgreSQL on the same
// workload (cheesedb/internal/benchgen). It is a separate module so the
// main module stays free of dependencies. How to run it: BENCHMARKS.md.
//
// Both SQL databases store the ledger the way a developer would on top of a
// general database: one table of nodes, the legs and their parents in two
// more tables, and the same counter algorithm cheesedb uses (deadParents,
// aliveLegs), run as SQL statements inside one transaction.
package bench

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/mattn/go-sqlite3"

	"github.com/govinargo-labs/cheesedb/internal/benchgen"
)

var sizes = []int{10_000, 100_000, 1_000_000}

func sizeName(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%dM", n/1_000_000)
	}
	return fmt.Sprintf("%dk", n/1000)
}

// engine is one SQL database holding one dataset.
type engine struct {
	name   string // "sqlite" or "postgres"
	db     *sql.DB
	open   func() (*sql.DB, error) // a fresh connection, for the startup benchmark
	nextID uint64                  // ids are assigned here, as cheesedb's log does
}

func (e *engine) pg() bool { return e.name == "postgres" }

// q rewrites ? placeholders as $1, $2, ... for PostgreSQL.
func (e *engine) q(s string) string {
	if !e.pg() {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func schema(pg bool) []string {
	id, blob := "INTEGER", "BLOB"
	if pg {
		id, blob = "BIGINT", "BYTEA"
	}
	return []string{
		"CREATE TABLE nodes (id " + id + " PRIMARY KEY, kind SMALLINT NOT NULL, dead SMALLINT NOT NULL, alive_legs INTEGER NOT NULL, source_type TEXT, data " + blob + ")",
		"CREATE TABLE legs (memory_id " + id + " NOT NULL, leg_no INTEGER NOT NULL, dead_parents INTEGER NOT NULL, PRIMARY KEY (memory_id, leg_no))",
		// Primary key starts with parent_id, so "children of x" is an index lookup.
		"CREATE TABLE leg_parents (parent_id " + id + " NOT NULL, memory_id " + id + " NOT NULL, leg_no INTEGER NOT NULL, PRIMARY KEY (parent_id, memory_id, leg_no))",
		"CREATE TABLE recalls (id " + id + " PRIMARY KEY, target " + id + " NOT NULL, mode SMALLINT NOT NULL, reason SMALLINT NOT NULL)",
	}
}

// openSQLite opens a database file with durability matching cheesedb's:
// WAL with a full sync on every commit, and F_FULLFSYNC on macOS (which
// Go's File.Sync, and so cheesedb, uses).
func openSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // PRAGMAs are per connection; keep exactly one
	for _, p := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA fullfsync=ON",
		"PRAGMA checkpoint_fullfsync=ON",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

// Datasets are built once per run and shared by every benchmark.
var cache struct {
	sync.Mutex
	root    string
	engines map[string]*engine
	pgDBs   []string
}

func TestMain(m *testing.M) {
	code := m.Run()
	for _, e := range cache.engines {
		e.db.Close()
	}
	if len(cache.pgDBs) > 0 {
		if admin, err := sql.Open("pgx", os.Getenv("CHEESEDB_PG")+" dbname=postgres"); err == nil {
			for _, name := range cache.pgDBs {
				admin.Exec("DROP DATABASE IF EXISTS " + name)
			}
			admin.Close()
		}
	}
	if cache.root != "" {
		os.RemoveAll(cache.root)
	}
	os.Exit(code)
}

// getEngine returns the named database loaded with a dataset of size
// memories, building it on first use.
func getEngine(b *testing.B, name string, size int) *engine {
	cache.Lock()
	defer cache.Unlock()
	key := name + "/" + sizeName(size)
	if e, ok := cache.engines[key]; ok {
		return e
	}
	if cache.engines == nil {
		cache.engines = map[string]*engine{}
	}
	var e *engine
	switch name {
	case "sqlite":
		if cache.root == "" {
			root, err := os.MkdirTemp("", "cheesedb-sqlbench-")
			if err != nil {
				b.Fatal(err)
			}
			cache.root = root
		}
		path := filepath.Join(cache.root, sizeName(size)+".db")
		e = &engine{name: name, open: func() (*sql.DB, error) { return openSQLite(path) }}
	case "postgres":
		base := os.Getenv("CHEESEDB_PG")
		if base == "" {
			b.Skip("PostgreSQL: set CHEESEDB_PG (see BENCHMARKS.md)")
		}
		dbName := "cheesedb_bench_" + strings.ToLower(sizeName(size))
		admin, err := sql.Open("pgx", base+" dbname=postgres")
		if err != nil {
			b.Fatal(err)
		}
		for _, s := range []string{"DROP DATABASE IF EXISTS " + dbName, "CREATE DATABASE " + dbName} {
			if _, err := admin.Exec(s); err != nil {
				b.Fatal(err)
			}
		}
		admin.Close()
		cache.pgDBs = append(cache.pgDBs, dbName)
		e = &engine{name: name, open: func() (*sql.DB, error) {
			db, err := sql.Open("pgx", base+" dbname="+dbName)
			if db != nil {
				db.SetMaxOpenConns(1) // one client, like cheesedb's one writer
			}
			return db, err
		}}
	default:
		b.Fatalf("unknown engine %q", name)
	}

	db, err := e.open()
	if err != nil {
		b.Fatal(err)
	}
	e.db, e.nextID = db, 1
	for _, s := range schema(e.pg()) {
		if _, err := db.Exec(s); err != nil {
			b.Fatal(err)
		}
	}
	if err := e.bulk(benchgen.Dataset(size)); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec("ANALYZE"); err != nil {
		b.Fatal(err)
	}
	if !e.pg() {
		if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
			b.Fatal(err)
		}
	}
	cache.engines[key] = e
	return e
}

// bulk writes ops in one transaction with multi-row INSERTs. It is the
// fastest honest way to load a dataset, and is never timed.
func (e *engine) bulk(ops []benchgen.Op) error {
	tx, err := e.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	nodes := &inserter{e: e, tx: tx, table: "nodes (id, kind, dead, alive_legs, source_type, data)", ncols: 6}
	legs := &inserter{e: e, tx: tx, table: "legs (memory_id, leg_no, dead_parents)", ncols: 3}
	parents := &inserter{e: e, tx: tx, table: "leg_parents (parent_id, memory_id, leg_no)", ncols: 3}
	for i, op := range ops {
		id := int64(e.nextID) + int64(i)
		if op.Lot {
			err = nodes.add(id, 1, 0, 0, benchgen.SourceType, benchgen.Content)
		} else {
			err = nodes.add(id, 2, 0, len(op.Legs), nil, benchgen.Content)
			for li, leg := range op.Legs {
				err = errors.Join(err, legs.add(id, li, 0))
				for _, p := range leg {
					err = errors.Join(err, parents.add(int64(p), id, li))
				}
			}
		}
		if err != nil {
			return err
		}
	}
	if err := errors.Join(nodes.flush(), legs.flush(), parents.flush()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	e.nextID += uint64(len(ops))
	return nil
}

// inserter batches rows into multi-row INSERT statements of 500 rows.
type inserter struct {
	e     *engine
	tx    *sql.Tx
	table string
	ncols int
	args  []any
	rows  int
}

func (in *inserter) add(vals ...any) error {
	in.args = append(in.args, vals...)
	in.rows++
	if in.rows == 500 {
		return in.flush()
	}
	return nil
}

func (in *inserter) flush() error {
	if in.rows == 0 {
		return nil
	}
	row := "(" + strings.TrimSuffix(strings.Repeat("?, ", in.ncols), ", ") + ")"
	stmt := "INSERT INTO " + in.table + " VALUES " + strings.TrimSuffix(strings.Repeat(row+", ", in.rows), ", ")
	_, err := in.tx.Exec(in.e.q(stmt), in.args...)
	in.args, in.rows = in.args[:0], 0
	return err
}

var errDeadParent = errors.New("parent is dead")

// remember is cheesedb's Remember in SQL: check each parent is an alive
// lot or memory, insert the memory with its legs, commit (one durable write).
func (e *engine) remember(legs [][]uint64) (uint64, error) {
	tx, err := e.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, leg := range legs {
		for _, p := range leg {
			var dead int
			if err := tx.QueryRow(e.q("SELECT dead FROM nodes WHERE id = ? AND kind IN (1, 2)"), int64(p)).Scan(&dead); err != nil {
				return 0, err
			}
			if dead != 0 {
				return 0, errDeadParent
			}
		}
	}
	id := int64(e.nextID)
	if _, err := tx.Exec(e.q("INSERT INTO nodes (id, kind, dead, alive_legs, source_type, data) VALUES (?, 2, 0, ?, NULL, ?)"), id, len(legs), benchgen.Content); err != nil {
		return 0, err
	}
	for li, leg := range legs {
		if _, err := tx.Exec(e.q("INSERT INTO legs (memory_id, leg_no, dead_parents) VALUES (?, ?, 0)"), id, li); err != nil {
			return 0, err
		}
		for _, p := range leg {
			if _, err := tx.Exec(e.q("INSERT INTO leg_parents (parent_id, memory_id, leg_no) VALUES (?, ?, ?)"), int64(p), id, li); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	e.nextID++
	return uint64(id), nil
}

// recallFair is cheesedb's Fair recall in SQL, in one transaction: record
// the recall, then walk down the children with a queue, updating the same
// counters cheesedb keeps in memory. Returns how many memories died.
func (e *engine) recallFair(target uint64) (int, error) {
	tx, err := e.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(e.q("INSERT INTO recalls (id, target, mode, reason) VALUES (?, ?, 1, 1)"), int64(e.nextID), int64(target)); err != nil {
		return 0, err
	}
	res, err := tx.Exec(e.q("UPDATE nodes SET dead = 1 WHERE id = ? AND dead = 0"), int64(target))
	if err != nil {
		return 0, err
	}
	removed := 0
	if n, _ := res.RowsAffected(); n == 1 {
		queue := []int64{int64(target)}
		for len(queue) > 0 {
			x := queue[0]
			queue = queue[1:]
			// Read every child first: the next statements need the
			// connection, which an open result set would still hold.
			type ref struct{ memory, leg int64 }
			var refs []ref
			rows, err := tx.Query(e.q("SELECT memory_id, leg_no FROM leg_parents WHERE parent_id = ?"), x)
			if err != nil {
				return 0, err
			}
			for rows.Next() {
				var r ref
				if err := rows.Scan(&r.memory, &r.leg); err != nil {
					rows.Close()
					return 0, err
				}
				refs = append(refs, r)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, err
			}
			for _, r := range refs {
				var deadParents int
				if err := tx.QueryRow(e.q("UPDATE legs SET dead_parents = dead_parents + 1 WHERE memory_id = ? AND leg_no = ? RETURNING dead_parents"), r.memory, r.leg).Scan(&deadParents); err != nil {
					return 0, err
				}
				if deadParents != 1 {
					continue // that leg was already dead
				}
				var aliveLegs, dead int
				if err := tx.QueryRow(e.q("UPDATE nodes SET alive_legs = alive_legs - 1 WHERE id = ? RETURNING alive_legs, dead"), r.memory).Scan(&aliveLegs, &dead); err != nil {
					return 0, err
				}
				if aliveLegs == 0 && dead == 0 {
					if _, err := tx.Exec(e.q("UPDATE nodes SET dead = 1 WHERE id = ?"), r.memory); err != nil {
						return 0, err
					}
					removed++
					queue = append(queue, r.memory)
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	e.nextID++
	return removed, nil
}

// get is cheesedb's Get: one lot or memory by id.
func (e *engine) get(db *sql.DB, id uint64) error {
	var kind, dead int
	var data []byte
	return db.QueryRow(e.q("SELECT kind, dead, data FROM nodes WHERE id = ? AND kind IN (1, 2)"), int64(id)).Scan(&kind, &dead, &data)
}

// diskBytes is the size of the dataset on disk: the database file for
// SQLite (after a checkpoint, so the WAL is empty), the four tables with
// their indexes for PostgreSQL.
func (e *engine) diskBytes(b *testing.B) int64 {
	if e.pg() {
		var n int64
		err := e.db.QueryRow("SELECT pg_total_relation_size('nodes') + pg_total_relation_size('legs') + pg_total_relation_size('leg_parents') + pg_total_relation_size('recalls')").Scan(&n)
		if err != nil {
			b.Fatal(err)
		}
		return n
	}
	if _, err := e.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		b.Fatal(err)
	}
	var pages, pageSize int64
	if err := e.db.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		b.Fatal(err)
	}
	if err := e.db.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		b.Fatal(err)
	}
	return pages * pageSize
}
