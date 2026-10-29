# cheesedb architecture

How the engine is built, as it stands now.

![cheesedb internals: in-memory state rebuilt by replay, append-only
segments on disk, and the three-part record](docs/architecture.png)

The picture must match this file. When a design change affects anything shown in it, the picture gets updated too.

The `cheesedb` command (`cmd/cheesedb/`) is a command-line tool and LLM demo for trying everything.

## 1. The model in one paragraph

An agent stores **memories**. Each memory records the **legs** it was learned from. A leg is a list of parents (sources called **lots**, or other memories), and it holds only while ALL of its parents are alive. A memory lives while ANY leg holds (Fair recall), or dies if a recalled node appears anywhere in its ancestry (Strict recall). Recalling a source therefore removes exactly what depended on it.

The caller reaches an agent's data only through a handle opened with a token:

```
cheesedb.Open(dir) -> *Store             the folder holding all agents
  CreateAgent(name) -> Token          returns the token once; the store keeps only its SHA-256
  OpenAgent(token)  -> *Agent         the only way to reach an agent
  DeleteAgent(token)                  removes the agent's folder; refused while it is open

*Agent (no method takes an agent name or id):
  AddLot      register a source
  Remember    store a memory with its legs
  Recall      recall a lot or memory, Fair or Strict
  Active      the memories currently alive
  Get         one lot or memory: kind, data, alive
  Verify      check the log on disk; returns the head fingerprint
  Compact     erase the content of every dead record
  Close
```

Signatures (package `cheesedb`, wrapping `internal/ledger`):
- `AddLot(sourceType string, data []byte) (ID, error)`
- `Remember(data []byte, legs ...Leg) (ID, error)`
- `Recall(id ID, mode Mode, reason Reason) ([]ID, error)`: returns the memories it newly removed, in id order.
- `Active() ([]ID, error)`
- `Get(id ID) (Item, error)`
- `Verify() ([32]byte, error)`

The example in `README.md` runs as a test (`example_test.go`).

## 2. Packages

```
cheesedb/             module github.com/govinargo-labs/cheesedb
  go.mod
  *.go                public API, package cheesedb: a folder of agents, token-based
                      agent handles (cheesedb.go, lock_*.go)
  internal/record/    one record <-> bytes. No files; pure functions.
  internal/logfile/   the segments: Append + fsync, replay on Open, torn-tail recovery.
  internal/ledger/    memory logic: lots, legs, recall. state.go is the pure in-memory graph.
  internal/benchgen/  the shared benchmark workload
  bench/              separate module: SQLite and PostgreSQL comparison
  cmd/cheesedb/       the `cheesedb` command: tries every feature, plus an LLM demo
                      (`go install ./cmd/cheesedb`, then `cheesedb help`). Keeps its
                      data in ~/.cheesedb or $CHEESEDB_DIR. Not part of the library.
```

Encoding is kept separate from file I/O so every byte of the format can be tested without touching the disk. `internal/` prevents other modules importing the building blocks directly.

### How a request flows through the packages

**The store layer** comes first. Every call on an agent handle passes straight through to that agent's ledger:

```
Agent handle (cheesedb)                          one per OpenAgent call
   0. handle or store closed? → ErrClosed      no lock taken: agents never wait on each other
  │
  ▼
That agent's ledger: the flows below
```

**Open an agent: `OpenAgent(token)`**

```
Program
  │  OpenAgent(token)
  ▼
Store
   1. key = SHA-256(token)
   2. take the store lock
   3. key in the map of agents?              no → ErrBadToken
   4. already open? → reuse its ledger       (one ledger per agent: one writer)
      not open?     → ledger.Open(folder)    replays the log (the Open flow below)
   5. count one more handle, release the lock
  │
  ▼
Program gets an Agent handle
```

**Create and delete: `CreateAgent(name)`, `DeleteAgent(token)`**

```
CreateAgent(name)                              DeleteAgent(token)
   1. name 1-127 bytes?  no → ErrBadName          1. key = SHA-256(token); take the store lock
   2. folder = hex(name); take the store lock     2. unknown key → ErrBadToken
   3. folder exists?  yes → ErrAgentExists        3. agent open?  yes → ErrAgentInUse
   4. token = 32 random bytes                     4. rename folder → .tmp-<random>, fsync ──► disk
   5. .tmp-<random>/agent.key = SHA-256(token)    5. forget the key; remove the temporary folder
      fsync, rename into place, fsync ──────► disk
   6. remember key → folder; return the token
```

**Write: `Remember`** (`AddLot` and `Recall` follow the same path)

```
Agent
  │  Remember(data, legs)
  ▼
Ledger (ledger.go, structure.go, state.go)     runs in the caller's goroutine
   1. canonicalLegs: check and sort the legs   bad legs → error, nothing written
   2. take the write lock                      one write per agent at a time
   3. state.checkParents                       unknown or dead parent → error
   4. encodeMemory: legs → structure bytes
  │  AppendBatch(entry)
  ▼
Log (logfile.go)                               still the caller's goroutine
   5. record.New: salt + contentDigest
   6. hand the request to the committer, wait
  │
  ▼
Committer goroutine (one per open log)
   7. gather every request already waiting into one batch
   8. segment full? → write its .seal, create the next .seg ──────► disk
   9. per record: id, prevHash, ChainHash, Encode (crc)
  10. one write + one fsync of the active segment ────────────────► disk
  11. update the offset table and lastHash
  │  reply: id
  ▼
Ledger
  12. state.addMemory: family tree, child links, counters
      (Recall: state.recall walks the children, returns the removed ids)
  13. release the write lock
  │
  ▼
Agent gets the id
```

Disk first, memory second: the state changes only after the fsync has returned, so a crash can never leave memory ahead of the disk.

**Write: `AddLot`** (the same path as `Remember`, minus the leg checks)

```
Agent
  │  AddLot(sourceType, data)
  ▼
Ledger
   1. checkSourceType: 1-255 bytes of UTF-8       bad label → error
   2. take the write lock
   3. encodeLot: label → structure bytes
  │  AppendBatch(entry)
  ▼
Log + committer: steps 5-11 of Remember (salt, digest, id, chain, write, fsync)
  │  reply: id
  ▼
Ledger
   4. state.addLot
   5. release the write lock
  │
  ▼
Agent gets the lot's id
```

**Write: `Recall`**

```
Agent
  │  Recall(id, Fair|Strict, untrusted|erase)
  ▼
Ledger
   1. check mode and reason                       bad value → error
   2. take the write lock
   3. state: is id a lot or memory?               no → ErrUnknownID
   4. encodeRecall: target, mode, reason → structure bytes
  │  AppendBatch(entry)
  ▼
Log + committer: steps 5-11 of Remember. The RECALL record is on disk;
nothing is deleted.
  │  reply: the RECALL record's id
  ▼
Ledger
   5. state.recall:
        mark the target dead (die)
        walk down the child links with a queue:
          die() updates each child's leg counters
          Fair: a child dies when no leg is left alive
          Strict: every descendant dies
   6. reason Erase only:
        ids = the target and every dead lot or memory below it that still has content
        Log.Redact(ids) in the committer: rewrite each segment holding them
          without their content (copy, fsync, rename, fsync folder) ───► disk
        mark them redacted in the state
   7. release the write lock
  │
  ▼
Agent gets the ids of the memories that died
(Untrusted: their content stays on disk until Compact)
```

**Maintain: `Compact`** (only when called; automatic compaction is planned)

```
Agent
  │  Compact()
  ▼
Ledger
   1. take the write lock
   2. ids = every dead lot or memory whose content is still on disk
   3. Log.Redact(ids): same rewrite as an Erase recall ─────────────► disk
   4. mark them redacted
  │
  ▼
Agent gets how many records were erased (family tree, counters and head unchanged)
```

**Read: `Active`** (memory only, no disk)

```
Agent
  │  Active()
  ▼
Ledger
   1. state (read lock): every memory not marked dead, in id order
  │
  ▼
Agent gets a list of ids
```

**Check: `Verify`** (re-reads everything; changes nothing; returns the head fingerprint)

```
Agent
  │  Verify()
  ▼
Ledger → Log.Verify
   1. note the segments and how many records exist right now
      (writes may continue; only these records are checked)
   2. for every segment, read every record from disk ───────────────► disk
        crc + content digest (Decode) → id is next → prevHash links the chain
   3. for every sealed segment:
        no extra bytes after its last record
        matches its .seal file (first id, last id, last chain hash) ◄── disk
   4. the count matches what the log holds
  │
  ▼
Agent gets the head (newest record's chain hash), or ErrCorrupt naming the first problem
```

**Stop: `Close`**

```
Agent
  │  Close()
  ▼
Ledger → Log.Close
   1. tell the committer to stop
   2. wait for it to finish the batch in hand
   3. close every segment file
  │
  ▼
Done. Nothing to flush: every acknowledged record was already fsynced.
```

**Read: `Get`** (`Active` only reads the state)

```
Agent
  │  Get(id)
  ▼
Ledger
   1. state (read lock): is id a lot or memory? alive?
  │  Read(id)
  ▼
Log
   2. offset table: id → (segment, offset)
   3. pread the record from that segment ─────────────────────────► disk
   4. record.Decode: crc and content digest checked
  │
  ▼
Agent gets kind, data, alive
```

**Startup: `Open`**

```
Ledger.Open
  │  logfile.Open(dir, opts, apply)
  ▼
Log
   1. check the options, create the folder if new
   2. list the .seg and .seal files; numbering must be 1..N
   3. open the segments (sealed read-only, active read-write)
   4. replay every record, in order:
        crc + digest → id is next → prevHash links the chain
        → ledger's state.apply rebuilds the family tree
        → offset table records where it lives
   5. sealed segment ends: compare with its .seal
      active segment ends: cut a torn tail, if any
      last segment already sealed: create the next one
   6. start the committer goroutine
  │
  ▼
Ledger ready (Report says what was replayed or cut)
```

## 3. Storage: append-only segments on disk, state rebuilt in memory

### On disk: a store folder, one folder of segments per agent

```
<store dir>/                0700, chosen by the developer (no default path in v1)
  LOCK                      held by the one open Store (an OS file lock); empty
  616c696365/               hex of the agent's name ("alice"), 0700
    agent.key               SHA-256 of the agent's token (never the token): version, hash, CRC-32C
    000001.seg  000001.seal ...
  .tmp-<random>/            only during a create or delete; Open removes leftovers
```

- Each agent has its own directory, named by the hex of its name. Hex uses only `0-9a-f`, so no `/` or `..` can escape the store folder, and distinct names stay distinct on case-insensitive file systems (`A` = `41`, `a` = `61`).
  - Names are 1-127 bytes (254 hex characters, under the 255-byte file name limit) and are compared byte by byte.
  - The picture labels the directory `alice/` for readability; on disk it is `616c696365/`.
- **Creating an agent** builds its folder under a short random temporary name (`.tmp-` plus 16 hex characters; a name derived from the agent's would exceed 255 bytes), writes and fsyncs `agent.key`, then renames the folder into place. A crash leaves no agent or a complete one.
- **Deleting an agent** renames its folder to a temporary name (the agent disappears in one atomic step), then removes it.
- **One Store per folder (file lock).** `cheesedb.Open` first takes an exclusive OS lock on `LOCK`, without waiting. A second Open, from another process or the same one, gets `ErrLocked` until `Close`. The lock comes before anything else because Open deletes leftover `.tmp-` folders, which could otherwise be another process's create still in progress.
  - macOS, Linux and the BSDs use `flock(2)`. Windows opens the file with sharing switched off, its own exclusive lock. Both are standard library calls.
  - The lock belongs to the open file, so the OS releases it when the process exits or crashes (even `kill -9`). No stale lock is ever left to clean up by hand; the empty `LOCK` file stays.
  - Systems with neither (for example Solaris, AIX) get no lock (README Limits).
  - Locks on network file systems (NFS, SMB) may not work; use a local disk.
- **`cheesedb.Open`** then reads every `agent.key` into an in-memory map from token hash to folder. A missing or damaged key is `ErrCorrupt` for the whole store, rather than silently hiding that agent.
- **One ledger per agent:** the store keeps a list of open agents, guarded by one in-memory lock that is held only to create, open, close or delete an agent. Opening an agent twice gives two handles on one ledger, so there is never a second writer. An agent's reads and writes never take the store's lock.
  - The first open of an agent replays its log while holding that lock, so other agents' opens wait during it (their reads and writes do not).
- Inside it, the agent's log is split into numbered **segment** files: `000001.seg`, `000002.seg`, ... Together they form ONE log: ids and the hash chain run on from one segment into the next.
- Exactly one segment is **active**: the highest-numbered one. New records are only ever appended to its end ("append + fsync" in the picture).
- When the next batch would not fit in the active segment, the segment is **sealed**: nothing is ever appended to it again. The next segment is created, and the directory is fsynced so the new file survives a crash. A batch never spans two segments.
- Sealing writes a **seal file** next to the segment (`000001.seal` for `000001.seg`) BEFORE the next segment is created; see "Seal files" below. The segment's records need no extra fsync, because every batch was already fsynced. Sealed segments are opened read-only.
- If creating the next segment fails, the log is broken until reopened, the same rule as a failed write (section 6).
- Records are never edited in place. The only rewrite is redaction (section 7), and it rewrites a single segment.
- Record types: `1 = LOT`, `2 = MEMORY`, `3 = RECALL`.
- IDs are 1, 2, 3, ... per agent and assigned by the log, never by a caller. A record's parents always have smaller IDs, so ID order is a topological order.

Why segments (real-world term: a **segmented log**, as in Kafka or etcd's WAL):
- Redaction rewrites one segment instead of the agent's whole history.
- A crash can only tear the active segment; sealed ones were fully fsynced before the next one existed.
- Later (v3), snapshots and log shipping can work segment by segment.

**Segment size** (`Options.SegmentSize`): default **64 MiB**.
- Minimum `MaxBatchBytes` (1 MiB + 8), so any batch fits in a fresh segment and no segment ever grows past its limit.
- Maximum 1 GiB, so an offset inside a segment fits in a `uint32`.
- Invalid options are refused before anything is created on disk.

**Open files:** every segment file stays open while the log is open, so `Read` is one `pread`. Many agents times many segments can hit the OS open-file limit (often 256 per process on macOS, see `ulimit -n`). Fine for now; a later fix is an LRU cache of open segment files, opening sealed ones on demand.

**Known limit:** deleting the newest segment(s), or every segment, leaves a log that looks valid, just shorter. Nothing inside the files can prove records are missing from the end. That needs a head hash kept elsewhere (v2: signed checkpoints sent to an external witness).

**Why the log always starts at segment 1:** records are never removed, so no segment ever becomes empty enough to delete. A log that starts above 1 has lost its first files, and `Open` returns `ErrCorrupt`.

**Future (v3): deleting old segments.** Once snapshots exist, segments older than the snapshot could be deleted (log truncation, as PostgreSQL does with WAL files after a checkpoint, or Kafka by retention). Then:
- numbering may start above 1, but must still be consecutive;
- a deliberate cleanup must be provable, or it looks exactly like lost files. The snapshot (or a marker written before deleting) records where the log now starts: first segment, first id, and the chain hash before it. Replay checks the first segment against it. Seal files help, since they record each segment's first and last id.

**Seal files.** Each sealed segment has a small separate file recording what it holds, so it can be checked on its own. It is a separate file rather than a footer, so segments stay "records only" and replay needs no special case.

```
offset  size  field
0       1     version        currently 1
1       8     first id
9       8     last id
17      32    chain hash of the segment's last record
49      4     CRC-32C over bytes 0-48
```

- **Written atomically:** temporary file, fsync, rename, fsync the directory. A crash leaves the whole seal or none.
- **Written before the next segment is created.** So a crash between the two leaves a last segment that already has a seal. `Open` treats it as sealed, creates the next segment, and sets `Report.ResumedRollover`.
- **Checked on every `Open` and by `Verify`.** Every segment except the last must have a seal matching its first id, last id and last chain hash. A seal with no segment is also `ErrCorrupt`.
- **Redaction never invalidates a seal,** because it does not change chain hashes.

### In memory: rebuilt by replay, never persisted

The current state (which memories are alive) is NOT stored on disk. On open, the engine **replays** every segment in order and rebuilds the state in memory. This pattern is called event sourcing. (v3 snapshots will save this state to shorten replay.)

| Structure | Holds | Used for |
|---|---|---|
| Family tree | each node's legs (each leg a list of parent ids) and its children | walking from a recalled node to everything that depended on it |
| Leg counters | per memory: how many legs are still alive, and its status (alive or dead) | Fair recall: a memory dies when its count reaches 0 |
| Offset table | a dense slice indexed by `id - 1`, each entry `(segment, offset)` | reading any record from disk in O(1) |

Only ids, links, counters and positions are kept in memory. Memory content stays on disk and is read on demand through the offset table.

The picture shows the reference example after `Recall(C, Fair)`. Lots A=1, B=2, C=3. M1=4 has two legs, `[A]` or `[B]`. M2=5 has one leg `[C]`. M3=6 has one leg needing M1 AND M2.

| id | memory | legs alive | status | why |
|---|---|---|---|---|
| 4 | M1 | 2 of 2 | alive | C is not in either leg |
| 5 | M2 | 0 of 1 | dead | its only leg needed C |
| 6 | M3 | 0 of 1 | dead | its only leg needed M2 |

The offset table in the picture: `1 -> (1, 0)`, `2 -> (1, 312)`, `3 -> (1, 590)`, `4 -> (2, 0)`. Record 1 is 312 bytes long, so record 2 starts at byte 312 of segment 1. Record 4 is the first record in segment 2. (These numbers are only illustrative; real segments hold far more than three records.)

In Go (`internal/ledger/state.go`):

```go
nodes []node  // nodes[id-1]; every record has one, RECALLs too, so lookup stays a slice index
type node     struct { kind; dead bool; legs []leg; aliveLegs uint32; children []childRef }
type leg      struct { parents []ID; deadParents uint32 }  // alive while deadParents == 0
type childRef struct { child ID; leg uint32 }               // "child depends on me through its leg number leg"
```

**Worked example: the state after `Recall(C, Fair)`.** This is exactly what sits in memory, written as Go values. Kind 1 = lot, 2 = memory, 3 = recall. Legs are counted from 0 in the code.

```go
nodes = []node{
  /* id 1, A  */ {kind: 1, dead: false, children: []childRef{{child: 4, leg: 0}}},
  /* id 2, B  */ {kind: 1, dead: false, children: []childRef{{child: 4, leg: 1}}},
  /* id 3, C  */ {kind: 1, dead: true,  children: []childRef{{child: 5, leg: 0}}},

  /* id 4, M1 */ {kind: 2, dead: false,
                  legs: []leg{{parents: []ID{1}, deadParents: 0},
                              {parents: []ID{2}, deadParents: 0}},
                  aliveLegs: 2,
                  children: []childRef{{child: 6, leg: 0}}},

  /* id 5, M2 */ {kind: 2, dead: true,
                  legs: []leg{{parents: []ID{3}, deadParents: 1}},
                  aliveLegs: 0,
                  children: []childRef{{child: 6, leg: 0}}},

  /* id 6, M3 */ {kind: 2, dead: true,
                  legs: []leg{{parents: []ID{4, 5}, deadParents: 1}},
                  aliveLegs: 0},

  /* id 7     */ {kind: 3},   // the RECALL record
}
```

How to read it:
- The list position is id - 1: `nodes[0]` is id 1, and `nodes[3]` is id 4 (M1).
- `{child: 4, leg: 0}` on A means "memory 4 was learned from me, through its leg 0".
- Lots never have legs, so their `legs` and `aliveLegs` stay empty and 0. Fields left out are empty or zero.
- How it got here:
  1. C died, so M2's leg 0 got `deadParents` 0 -> 1. That leg died, so M2's `aliveLegs` went 1 -> 0 and M2 died.
  2. In the same way, M3's leg 0 died and M3 died.
  3. M1 is not below C, so it was never visited.

**Why the counters** (`deadParents`, `aliveLegs`): without them, deciding whether a child survives means re-checking every parent of every leg. With them, each link touched costs one small update (engineering target 3: fast recall).

**Recall** walks down the child links with a queue, a to-do list of nodes still to visit (breadth-first search), never recursion. Recursion keeps one waiting call per level of depth; Go allows that up to 1 GB, but the queue has no depth limit at all (a 100,000-deep chain is tested). The work is proportional to the nodes reached, never a scan of everything.
- When a node dies, each child's leg that lists it gets `deadParents++`. On 0 -> 1 that leg has just died, so the child's `aliveLegs--`. Counters are updated in both modes.
- **Fair:** a child dies when `aliveLegs` reaches 0.
- **Strict:** every descendant dies. The walk also passes THROUGH nodes that are already dead, because an earlier Fair recall may have killed a node while one of its children survived on another leg.
- Recalling an already-recalled node is allowed: Fair then Strict on the same lot can remove more.

**Rules.**
- `Remember` refuses:
  - no legs, or an empty leg;
  - the same parent twice in one leg, or two identical legs;
  - an unknown id, or a RECALL record as a parent;
  - a dead parent.
- `Recall` accepts only a lot or memory.
- On replay, a record that breaks any of these rules is `ErrCorrupt`.

**One write at a time per agent.** `Remember` checks that its parents are alive and then appends. A write lock is held across check -> append -> state update, so a `Recall` can never slip in between. Readers use a separate read/write lock and never wait for an fsync.
- Cost: within one agent, writes do not share fsyncs.
- Provision for multiple writers later: `state` does no I/O and takes no locks, so its checks and updates could move into the committer and run in id order. Replay would then need a "born dead" rule for a memory whose parent died just before it.

## 4. Record format

A record is an 8-byte prefix (`length`, `crc`) followed by a body. The body has three parts, matching the picture:

| Part | Holds | On redaction |
|---|---|---|
| Fixed header | version, type, id, hashes, reserved fields | kept |
| Structural section | what replay needs to rebuild state: a MEMORY's legs, a RECALL's target, mode and reason, a LOT's source type | kept |
| Content section | 16-byte random salt, then the data (what the lot or memory says) | removed |

Splitting structure from content means erasing what a memory *says* never erases *where it came from*. After redaction, replay can still rebuild the family tree, and Strict recall can still walk ancestry through a redacted memory.

All integers little-endian (least significant byte first).

```
offset  size  field
0       4     length         bytes in the body (everything after crc)
4       4     crc32          CRC-32C over length + body
--- body part 1: fixed header, always 150 bytes ---
8       1     version        format version, currently 1
9       1     type           1=LOT 2=MEMORY 3=RECALL
10      8     id             monotonic uint64
18      32    prevHash       chain hash of the previous record (zeros for record 1)
50      4     keyID          reserved for encryption, 0 in v1
54      4     signerKeyID    reserved for signing, 0 in v1
58      64    signature      reserved for Ed25519, zeros in v1
122     32    contentDigest  SHA-256(content section)
154     4     structLen      size of the structural section in bytes
--- body part 2: structural section (kept forever) ---
158     s     structure      encoding per record type
--- body part 3: content section (removed by redaction) ---
158+s   n     content        16-byte random salt, then the data
```

- `content size = length - 150 - structLen`.
- Maximum body size: **1 MiB**. A larger length field is treated as damage.
- Per-record overhead: 158 bytes plus the 16-byte salt. 72 of those bytes are reserved fields, kept so v2 needs no format change.
- `structLen` sits at the END of the header, so adding it moved no earlier offset.
- The record package stores the structural section as opaque bytes. `internal/ledger` gives them meaning, with every integer a uvarint (variable length: small numbers take 1 byte):

```
LOT     source type text (1-255 bytes, UTF-8)
MEMORY  legCount, then per leg: parentCount, parent ids (sorted ascending)
RECALL  target id, mode (1 = Fair, 2 = Strict), reason (1 = untrusted, 2 = erase)
```

- Parent ids are absolute, not deltas: a record's own id is assigned by the committer after encoding.
- Each leg's parents are sorted, so each leg set has exactly one encoding.
- Decoding refuses anything the encoder never writes: trailing bytes, zero legs, an empty leg, unsorted or repeated parents, a parent id of 0 or not below the record's own id, identical legs, and counts larger than the bytes left.

### What each protection is for

| Mechanism | Catches | Does not catch |
|---|---|---|
| `crc32` (over length + body) | accidents: torn writes, flipped bits | deliberate edits (anyone can recompute a crc) |
| `contentDigest` | content changed without updating its digest | an edit that also rewrites the digest |
| chain hash | any edited, inserted, removed or reordered record, even with its crc and digest recomputed | the chain rebuilt after an edit; the newest record edited or removed (nothing points back to it). v2 adds signatures and an external witness. |

### Salt and digest

`contentDigest = SHA-256(salt || data)`, and the salt is the first 16 bytes of the content section.
- While content exists: verification recomputes the digest from the content and compares.
- Without the salt, anyone could guess content ("user lives in Pune"), hash it and compare with the digest. Once redacted, the salt is gone along with the content, so the digest reveals nothing.
- The salt is generated at write time and stored, so replay never needs randomness.
- Every live record has at least the salt in its content section, even if it has no data. So an empty content section always means redacted.

### Chain hash

Each record's **chain hash** (`Record.ChainHash()`, its "fingerprint") is SHA-256 over these fields, concatenated in on-disk order and encoded as on disk: version, type, id, prevHash, keyID, signerKeyID, contentDigest, structLen and the structural section.

- **Writing:** the committer puts the previous record's chain hash into each record's `prevHash` (zeros for record 1). Each record is therefore locked to everything before it.
- **`Open`:** replay recomputes every chain hash and compares it with the next record's `prevHash`. A mismatch is `ErrCorrupt`, never a torn tail, because a crash cannot produce a valid record with a wrong link. The cost is about 25% slower startup (`BENCHMARKS.md`).
- **`Verify()`:** re-reads every segment from disk and repeats all checks: crc, content digest, ids, chain and seals. It catches files changed after `Open`, and it works while appends continue. It returns the **head fingerprint** (the newest record's chain hash; zeros for an empty log).
- **Not detectable from the files alone:**
  - an edit followed by rebuilding every later link;
  - an edit or removal of the newest record(s), since nothing points back to them.

  In every such case the **head** (the newest record's chain hash) changes, so a copy of the head kept elsewhere catches it. v2 automates this with signatures and a witness. `TestLimitsOfTheChain` documents these limits.

This is what protects a memory's parents. The crc can be recomputed by anyone, and `contentDigest` never looks at the structure, so an edited leg list with a fresh crc still decodes. Only the chain hash changes. A test shows exactly this.
It does NOT cover length, crc, content or signature:
- length, crc and content change when a record is redacted, and the chain must survive that;
- the structural section never changes and replay trusts it, so the chain must protect it;
- the signature (v2) will sign this hash, so it cannot be part of it.

## 5. Reading a record

By id: look up `(segment, offset)` in the offset table, then read from that segment file at that offset (`pread`, Go's `ReadAt`).

1. Read 8 bytes: `length` and `crc`.
2. Check the length is possible (150 to 1 MiB).
3. Read `length` more bytes.
4. Recompute the crc over length + body and compare.
5. Check the fields mean something valid: known version and type, `structLen` fits inside the body, content empty or at least 16 bytes, digest matches.

## 6. Crashes and corruption

A record is **bad** if its header is short, its length runs past the end of the segment, or its crc fails.

Before an fsync completes, the disk may save a write's blocks in any order. So a crash during a batch can leave `[good][garbage][valid-looking record]`, and none of the records from the garbage onward were acknowledged. That can only happen at the end of the active segment.

| Situation | Meaning | Action |
|---|---|---|
| Active segment: bad region (first bad record to end of segment) no larger than one batch (`MaxBatchBytes` = 1 MiB + 8) | the unfinished last batch of a crash | truncate it, report how many bytes were dropped |
| Active segment: bad region larger than one batch | damage a crash cannot cause | `Open` returns `ErrCorrupt`, files left untouched |
| Any bad record in a sealed segment | damage a crash cannot cause (it was fsynced before the next segment existed) | `ErrCorrupt` |
| A missing segment number (e.g. `000001`, `000003`) | a lost file | `ErrCorrupt` |
| An empty segment that is not the last | rollover only happens when the active segment holds records | `ErrCorrupt` |
| An empty last segment | a crash right after a rollover | fine; the next record goes there |
| A valid record whose `prevHash` does not match the chain | an edited, removed, inserted or reordered record | `ErrCorrupt` |
| A sealed segment with no seal, a damaged seal, or one that does not match | lost or edited file | `ErrCorrupt` |
| A seal with no segment | the segment was deleted | `ErrCorrupt` |
| The last segment already has a seal | a crash after sealing, before the next segment was created | fine; `Open` creates the next segment (`Report.ResumedRollover`) |
| A read error from the OS (not damaged bytes) | the disk or OS failed | return the error; never truncate |
| A valid record with the wrong id, including segment N+1 not starting at segment N's last id + 1 | damage or a bug | `ErrCorrupt` |

Known ambiguity: real damage inside the last ~1 MiB of the active segment looks like a torn batch, so those records are dropped (and reported) instead of failing loudly. Batch commit markers could remove this later.

**Durability and group commit:** one committer goroutine per open log owns all writes.
1. Callers hand records to it and wait. Salt, digest and validation run in the caller's goroutine first.
2. It takes the first waiting request plus everything else already waiting, up to `MaxBatchBytes`.
3. If the batch does not fit in the active segment, it seals that segment (writes its seal file) and starts a new one first.
4. It assigns ids in order, links each record into the hash chain (`prevHash`), does one `write` to the active segment, then one `fsync`.
5. Only then does it answer each caller with its ids, and only then is the in-memory state updated. Nothing is acknowledged before the fsync that covers it has returned.

No timers: under light load a batch is one record, so there is no added delay; under load, requests pile up during the previous fsync and share the next one. `fsync` costs milliseconds while encoding costs nanoseconds, so this is the main throughput lever.

If a write, fsync or rollover fails, the log refuses all further writes until it is reopened. Retrying fsync is unsafe: the OS may already have dropped the unsaved data and report success the second time (PostgreSQL's "fsyncgate", 2018). Reopening replays the segments and finds out what really survived. When a segment file is created, its directory is fsynced too, so the file's existence survives a crash.

**Single writer:** one process writes an agent's log, enforced by the store's file lock (section 3): a second process cannot open the store at all.

## 7. Redaction

Redaction erases a record's content while keeping its place in the log, its structure and its link in the hash chain.

```
before:  [length|crc] [header] [structure: legs, ...] [salt + data]
after:   [length|crc] [header] [structure: legs, ...]
```

- Header and structural section kept; salt and data dropped; `length` and `crc` recomputed. The chain hash is unaffected.
- **Empty content means redacted.** A live record always has at least the salt, so no separate flag is needed and no contradictory state can exist.
- **How a segment is rewritten** (`internal/logfile/redact.go`). The record got shorter, so the segment that holds it is rewritten:
  1. copy it to `NNNNNN.seg.tmp` without the erased content; unchanged records are copied byte for byte;
  2. fsync the copy;
  3. rename it over the segment (atomic);
  4. fsync the directory;
  5. swap the open file and update the offsets of records that moved.

  A crash before the rename leaves a `.tmp` file that `Open` removes, and the original is intact. Other segments are not touched. A sealed segment stays sealed, and its seal stays valid, because first id, last id and last chain hash do not change.
- **The committer does the rewrite,** so it never overlaps with appends. `Read` holds the read lock through its `pread`, so the swap (under the write lock) never pulls a file or an offset out from under a read. `Verify` opens each segment by name, so it reads one consistent version even if a rewrite happens meanwhile.
- **Later records in that segment move to smaller offsets;** their offset table entries are updated. Their ids never change.
- **What gets erased.** `Recall(x, ..., Erase)` erases the content of x and of every **dead** lot or memory below x, through any leg, including ones that died earlier. Memories still **alive** through another source keep their content.
  - Example: erasing B with Fair leaves M1's text, because M1 is still learned from A.
  - `Recall` returns only after the content is gone from the live files.
  - `Get` then returns `Item.Redacted = true` and no data.
- **Untrusted recalls** keep their content until **`Compact()`**, which erases the content of every dead record that still has it. It runs only when called; **automatic compaction is planned for later** (roadmap). Compaction leaves the family tree, counters, liveness and the head fingerprint unchanged (`TestCompact`).
- **A crash between an Erase recall and its rewrite** is finished by the next `Open` (`Report.FinishedErase`). It finds the content still due with one pass in id order over the erase targets. Erasing is idempotent: erasing an already-erased record changes nothing.
- **Old backups, snapshots and copies still hold the content,** and SSDs may keep old blocks (README Limits).
- **v1 limit:** anyone with file access can redact a record, and `Verify` still passes. Signatures (v2) close this.

## 8. Distribution (future design, nothing built)

### Level 1: no master, one writer per agent (fits the current design, planned for v3)
- Each agent log (its directory of segments) is self-contained, so it is a natural **shard**. Agents are placed on nodes by consistent hashing of the agent ID; no central coordinator.
- Each agent log has one writer at a time and is replicated by **log shipping**. Matching head hashes prove replicas are identical.
- Needed later: a leader term for safe failover (a new record type, no format change), quorum durability, redaction propagated to every replica, a directory from token hash to node.
- Never split one agent's log across machines: recall cascades would hop between nodes.

### Level 2: many writers for one agent (leaderless, not planned)
- The current format cannot do this: dense IDs and a single chain would fork if two replicas wrote at once.
- The model suits it anyway: records only grow and recall is permanent, so replicas could merge by set union and compute identical liveness (a CRDT). The rule "Remember rejects dead parents" becomes "rejects parents known to be dead locally".
- It would need: IDs as (replica, sequence), the hash chain becoming a hash DAG, and replay in causal order.
- Only worth it for local-first agents (for example offline laptop plus server).
- Hedge: keep parent references as one `uint64` that could later hold replica bits.

### Rules that keep both options open (follow now)
1. Each log is self-contained: nothing refers to another agent's log.
2. Replay is deterministic: same log gives the same state; no clock or randomness during replay.
3. Records are identified by `id`, never by byte offset (offsets change after redaction).
4. Redaction is deterministic: same input gives the same output segment.
