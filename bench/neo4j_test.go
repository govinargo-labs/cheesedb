package bench

import (
	"context"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j"

	"github.com/govinargo-labs/cheesedb/internal/benchgen"
)

// Neo4j Community, as a graph database would hold the ledger: the way a
// developer who picked a graph database for this job would model it.
//
//	(parent:Node)-[:IN_LEG]->(leg:Leg)-[:OF]->(memory:Node)
//
// A leg is a node because it joins several parents (all needed) to one memory.
// Recall is written the graph way, one set-based query per level of the
// cascade, with no counters: a memory dies when none of its legs has all its
// parents alive. (The SQL versions are a statement-by-statement port of
// cheesedb's counter algorithm, so this one is a different, more natural
// algorithm for a graph; BENCHMARKS.md says so.)
//
// Start a server with bench/neo4j.sh, then run:
//
//	export CHEESEDB_NEO4J=bolt://127.0.0.1:7699
//	go test -run '^$' -bench Neo4j -benchmem -timeout 60m
//
// Disk use is not measured here: Community Edition has no way to force a
// checkpoint while running, so stop the server (a clean shutdown checkpoints)
// and measure data/databases/neo4j in the folder you gave neo4j.sh.
//
// Community Edition has one user database, so a run holds ONE dataset size:
// pick it with -bench '/1M' and start a fresh server folder for another.

var ctx = context.Background()

type graphDB struct {
	uri    string
	driver neo4j.Driver
	sess   neo4j.Session
	nextID uint64
	size   int
}

var graphCache struct {
	sync.Mutex
	g *graphDB
}

func newGraph(uri string) (*graphDB, error) {
	d, err := neo4j.NewDriver(uri, neo4j.NoAuth())
	if err != nil {
		return nil, err
	}
	if err := d.VerifyConnectivity(ctx); err != nil {
		d.Close(ctx)
		return nil, err
	}
	return &graphDB{uri: uri, driver: d, sess: d.NewSession(ctx, neo4j.SessionConfig{}), nextID: 1}, nil
}

func (g *graphDB) close() {
	g.sess.Close(ctx)
	g.driver.Close(ctx)
}

// getGraph returns the server loaded with size memories, building it on
// first use. A server that already holds data (from another size, or an
// earlier run) is refused rather than wiped.
func getGraph(b *testing.B, size int) *graphDB {
	cache := &graphCache
	cache.Lock()
	defer cache.Unlock()
	if cache.g != nil {
		if cache.g.size != size {
			b.Skipf("this Neo4j server holds the %s dataset; start a fresh one for %s (see neo4j_test.go)", sizeName(cache.g.size), sizeName(size))
		}
		return cache.g
	}
	uri := os.Getenv("CHEESEDB_NEO4J")
	if uri == "" {
		b.Skip("Neo4j: set CHEESEDB_NEO4J (see BENCHMARKS.md)")
	}
	g, err := newGraph(uri)
	if err != nil {
		b.Fatal(err)
	}
	if n, err := g.count(); err != nil || n != 0 {
		b.Fatalf("the Neo4j server must be empty (nodes: %d, err: %v); start a fresh folder with bench/neo4j.sh", n, err)
	}
	if err := g.schema(); err != nil {
		b.Fatal(err)
	}
	if err := g.bulk(benchgen.Dataset(size)); err != nil {
		b.Fatal(err)
	}
	g.size = size
	cache.g = g
	return g
}

func (g *graphDB) count() (int64, error) {
	res, err := g.sess.Run(ctx, "MATCH (n) RETURN count(n) AS n", nil)
	if err != nil {
		return 0, err
	}
	rec, err := res.Single(ctx)
	if err != nil {
		return 0, err
	}
	return rec.Values[0].(int64), nil
}

// schema creates the unique index on Node.id (so "node x" is an index
// lookup, like the primary key in the SQL versions) and waits for it.
func (g *graphDB) schema() error {
	for _, q := range []string{
		"CREATE CONSTRAINT node_id IF NOT EXISTS FOR (n:Node) REQUIRE n.id IS UNIQUE",
		"CALL db.awaitIndexes(600)",
	} {
		res, err := g.sess.Run(ctx, q, nil)
		if err != nil {
			return err
		}
		if _, err := res.Consume(ctx); err != nil {
			return err
		}
	}
	return nil
}

func exec(tx neo4j.ManagedTransaction, cypher string, params map[string]any) error {
	res, err := tx.Run(ctx, cypher, params)
	if err != nil {
		return err
	}
	_, err = res.Consume(ctx)
	return err
}

func (g *graphDB) write(work func(tx neo4j.ManagedTransaction) error) error {
	_, err := g.sess.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) { return nil, work(tx) })
	return err
}

const (
	createNodes = "UNWIND $nodes AS r CREATE (:Node {id: r.id, kind: r.kind, dead: 0, source_type: r.st, data: $data})"
	createLegs  = `UNWIND $legs AS r
MATCH (m:Node {id: r.mem})
CREATE (l:Leg {no: r.no})-[:OF]->(m)
WITH l, r
UNWIND r.parents AS pid
MATCH (p:Node {id: pid})
CREATE (p)-[:IN_LEG]->(l)`
)

// rows turns ops into query parameters; ids start at first.
func rows(first uint64, ops []benchgen.Op) (nodes, legs []any) {
	for i, op := range ops {
		id := int64(first) + int64(i)
		if op.Lot {
			nodes = append(nodes, map[string]any{"id": id, "kind": int64(1), "st": benchgen.SourceType})
			continue
		}
		nodes = append(nodes, map[string]any{"id": id, "kind": int64(2), "st": nil})
		for li, leg := range op.Legs {
			ps := make([]any, len(leg))
			for k, p := range leg {
				ps[k] = int64(p)
			}
			legs = append(legs, map[string]any{"mem": id, "no": int64(li), "parents": ps})
		}
	}
	return nodes, legs
}

// bulk loads ops in transactions of 2,000 ops. It is never timed.
func (g *graphDB) bulk(ops []benchgen.Op) error {
	const batch = 2000
	for start := 0; start < len(ops); start += batch {
		end := min(start+batch, len(ops))
		nodes, legs := rows(g.nextID+uint64(start), ops[start:end])
		err := g.write(func(tx neo4j.ManagedTransaction) error {
			if err := exec(tx, createNodes, map[string]any{"nodes": nodes, "data": benchgen.Content}); err != nil {
				return err
			}
			if len(legs) == 0 {
				return nil
			}
			return exec(tx, createLegs, map[string]any{"legs": legs})
		})
		if err != nil {
			return err
		}
	}
	g.nextID += uint64(len(ops))
	return nil
}

// remember is cheesedb's Remember in Cypher: check each parent is an alive
// lot or memory, create the memory with its legs, commit (one durable write).
func (g *graphDB) remember(legs [][]uint64) (uint64, error) {
	id := g.nextID
	err := g.write(func(tx neo4j.ManagedTransaction) error {
		var ids []any
		seen := map[uint64]bool{}
		for _, leg := range legs {
			for _, p := range leg {
				if !seen[p] {
					seen[p] = true
					ids = append(ids, int64(p))
				}
			}
		}
		res, err := tx.Run(ctx, "MATCH (p:Node) WHERE p.id IN $ids AND p.kind IN [1, 2] RETURN p.dead AS dead", map[string]any{"ids": ids})
		if err != nil {
			return err
		}
		recs, err := res.Collect(ctx)
		if err != nil {
			return err
		}
		if len(recs) != len(ids) {
			return errors.New("unknown parent")
		}
		for _, r := range recs {
			if r.Values[0].(int64) != 0 {
				return errDeadParent
			}
		}
		nodes, legRows := rows(id, []benchgen.Op{{Legs: legs}})
		if err := exec(tx, createNodes, map[string]any{"nodes": nodes, "data": benchgen.Content}); err != nil {
			return err
		}
		return exec(tx, createLegs, map[string]any{"legs": legRows})
	})
	if err != nil {
		return 0, err
	}
	g.nextID++
	return id, nil
}

// One level of a Fair recall. $f holds the nodes that just died. Look at
// every memory that has one of them in a leg, and keep those with no leg
// left whose parents are all alive. collect() finishes the check on all of
// them before SET changes anything, so a memory that dies at this level
// cannot affect another memory's check at the same level; it is handled at
// the next level instead.
const recallLevel = `MATCH (d:Node) WHERE d.id IN $f
MATCH (d)-[:IN_LEG]->(:Leg)-[:OF]->(m:Node {dead: 0})
WITH DISTINCT m
WHERE NOT EXISTS {
  MATCH (m)<-[:OF]-(l:Leg)
  WHERE NOT EXISTS { MATCH (l)<-[:IN_LEG]-(:Node {dead: 1}) }
}
WITH collect(m) AS ms
UNWIND ms AS m
SET m.dead = 1
RETURN collect(m.id) AS died`

// recallFair is cheesedb's Fair recall in Cypher, in one transaction: record
// the recall, mark the target dead, then repeat recallLevel until nothing
// more dies. Returns how many memories died.
func (g *graphDB) recallFair(target uint64) (int, error) {
	removed := 0
	err := g.write(func(tx neo4j.ManagedTransaction) error {
		removed = 0
		if err := exec(tx, "CREATE (:Recall {id: $id, target: $t, mode: 1, reason: 1})", map[string]any{"id": int64(g.nextID), "t": int64(target)}); err != nil {
			return err
		}
		res, err := tx.Run(ctx, "MATCH (t:Node {id: $t, dead: 0}) SET t.dead = 1 RETURN count(t) AS n", map[string]any{"t": int64(target)})
		if err != nil {
			return err
		}
		rec, err := res.Single(ctx)
		if err != nil {
			return err
		}
		if rec.Values[0].(int64) != 1 {
			return nil // already dead
		}
		frontier := []any{int64(target)}
		for len(frontier) > 0 {
			res, err := tx.Run(ctx, recallLevel, map[string]any{"f": frontier})
			if err != nil {
				return err
			}
			rec, err := res.Single(ctx)
			if err != nil {
				return err
			}
			frontier = rec.Values[0].([]any)
			removed += len(frontier)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	g.nextID++
	return removed, nil
}

const getQuery = "MATCH (n:Node {id: $id}) WHERE n.kind IN [1, 2] RETURN n.kind, n.dead, n.data"

// get is cheesedb's Get: one lot or memory by id.
func (g *graphDB) get(id uint64) error {
	res, err := g.sess.Run(ctx, getQuery, map[string]any{"id": int64(id)})
	if err != nil {
		return err
	}
	_, err = res.Single(ctx)
	return err
}

// openAndGet opens a fresh connection and reads one memory: the counterpart
// of the SQL startup benchmark.
func (g *graphDB) openAndGet() error {
	d, err := neo4j.NewDriver(g.uri, neo4j.NoAuth())
	if err != nil {
		return err
	}
	defer d.Close(ctx)
	s := d.NewSession(ctx, neo4j.SessionConfig{})
	defer s.Close(ctx)
	res, err := s.Run(ctx, getQuery, map[string]any{"id": int64(2)})
	if err != nil {
		return err
	}
	_, err = res.Single(ctx)
	return err
}

func BenchmarkNeo4jStartup(b *testing.B) {
	for _, size := range sizes {
		b.Run("neo4j/"+sizeName(size), func(b *testing.B) {
			g := getGraph(b, size)
			b.ReportAllocs()
			for b.Loop() {
				if err := g.openAndGet(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkNeo4jGet(b *testing.B) {
	for _, size := range sizes {
		b.Run("neo4j/"+sizeName(size), func(b *testing.B) {
			g := getGraph(b, size)
			n := int(g.nextID - 1)
			rng := rand.New(rand.NewPCG(1, 1))
			b.ReportAllocs()
			for b.Loop() {
				id := uint64(1 + rng.IntN(n))
				for !benchgen.IsMemory(id) {
					id++
				}
				if err := g.get(id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkNeo4jRemember(b *testing.B) {
	for _, size := range sizes {
		b.Run("neo4j/"+sizeName(size), func(b *testing.B) {
			g := getGraph(b, size)
			if err := g.bulk([]benchgen.Op{{Lot: true}}); err != nil {
				b.Fatal(err)
			}
			lot := g.nextID - 1
			b.ReportAllocs()
			for b.Loop() {
				if _, err := g.remember([][]uint64{{lot}}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNeo4jRecall is a durable Fair recall of a lot that 20 memories
// depend on; each iteration first adds a fresh probe, untimed.
func BenchmarkNeo4jRecall(b *testing.B) {
	for _, size := range sizes {
		b.Run("neo4j/"+sizeName(size), func(b *testing.B) { graphRecallProbe(b, size, 10) })
	}
}

// BenchmarkNeo4jRecallLarge removes 10,000 memories in one recall, at 1M.
func BenchmarkNeo4jRecallLarge(b *testing.B) {
	b.Run("neo4j/1M", func(b *testing.B) { graphRecallProbe(b, 1_000_000, 5000) })
}

func graphRecallProbe(b *testing.B, size, width int) {
	g := getGraph(b, size)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := g.nextID
		if err := g.bulk(benchgen.Probe(p, width)); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		removed, err := g.recallFair(p)
		if err != nil || removed != 2*width {
			b.Fatalf("removed %d, err %v; want %d", removed, err, 2*width)
		}
	}
}

// TestNeo4jRecallMatchesSQLite checks that the Cypher recall kills exactly
// the memories the SQL (counter) recall kills, over random targets on a
// dataset with deep cascades. It empties the server before and after, so run
// it on a throwaway server only, and not in the same run as the benchmarks.
func TestNeo4jRecallMatchesSQLite(t *testing.T) {
	uri := os.Getenv("CHEESEDB_NEO4J")
	if uri == "" {
		t.Skip("Neo4j: set CHEESEDB_NEO4J (see BENCHMARKS.md)")
	}
	g, err := newGraph(uri)
	if err != nil {
		t.Fatal(err)
	}
	defer g.close()
	wipe := func() {
		res, err := g.sess.Run(ctx, "MATCH (n) DETACH DELETE n", nil)
		if err == nil {
			_, err = res.Consume(ctx)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	wipe()
	defer wipe()
	if err := g.schema(); err != nil {
		t.Fatal(err)
	}

	ops := benchgen.Dataset(2000)
	if err := g.bulk(ops); err != nil {
		t.Fatal(err)
	}
	db, err := openSQLite(filepath.Join(t.TempDir(), "match.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	sq := &engine{name: "sqlite", db: db, nextID: 1}
	for _, s := range schema(false) {
		if _, err := db.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	if err := sq.bulk(ops); err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewPCG(7, 7))
	total := 0
	for range 40 {
		target := uint64(1 + rng.IntN(len(ops)))
		want, err := sq.recallFair(target)
		if err != nil {
			t.Fatal(err)
		}
		got, err := g.recallFair(target)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("recall of %d removed %d memories in Neo4j, %d in SQL", target, got, want)
		}
		total += want
	}
	if total < 50 {
		t.Fatalf("only %d memories died in total; the test is too gentle to prove anything", total)
	}

	var sqlDead []int64
	rowsSQL, err := db.Query("SELECT id FROM nodes WHERE dead = 1 ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rowsSQL.Next() {
		var id int64
		if err := rowsSQL.Scan(&id); err != nil {
			t.Fatal(err)
		}
		sqlDead = append(sqlDead, id)
	}
	rowsSQL.Close()
	res, err := g.sess.Run(ctx, "MATCH (n:Node {dead: 1}) RETURN n.id AS id ORDER BY id", nil)
	if err != nil {
		t.Fatal(err)
	}
	recs, err := res.Collect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var graphDead []int64
	for _, r := range recs {
		graphDead = append(graphDead, r.Values[0].(int64))
	}
	if !slices.Equal(sqlDead, graphDead) {
		t.Fatalf("the dead sets differ: SQL has %d, Neo4j has %d", len(sqlDead), len(graphDead))
	}
	t.Logf("40 random recalls on %d nodes: %d memories died, identical sets in both engines", len(ops), total)
}
