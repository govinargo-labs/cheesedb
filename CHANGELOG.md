# Changelog

cheesedb follows [semantic versioning](https://semver.org). Until v1.0.0, the API and the on-disk format may change between minor versions.

## Unreleased

- **Opening a large agent is about 8x faster: snapshots.** `Close` saves the agent's state in `state.snap` once 1,000 records are newer than the last snapshot, and opening loads it and replays only the records after it. At 1M memories, opening takes 90 ms instead of 728 ms ([BENCHMARKS.md](BENCHMARKS.md)). Every `cheesedb` command and MCP tool call opens the store again, so each pays less.
  - The log stays the truth. A snapshot that is damaged, from another agent, or older or newer than the log is thrown away, and the whole log is replayed as before. `Erase` and `Compact` delete the snapshot before they rewrite anything.
  - It holds no text: ids, flags and legs only.
  - `Report.Snapshot` says how many records came from the snapshot (0 means a full replay). `Report.Records` still counts every record.
  - **The guarantee that changed:** opening no longer re-hashes the records a snapshot covers. It checks each covered segment's size, seal, and first and last record. Damage or an edit elsewhere in an older segment is found by reading that record, or by `Verify`, which still checks everything. See README Limits.
  - Stores from earlier versions open unchanged (they have no snapshot). An earlier version can open a store with a snapshot: it ignores the file, and if it writes or erases, the next open notices the snapshot no longer fits and replays everything.

- **Security: built with Go 1.26.11.** Go 1.26.5 had known bugs in its HTTP server that let a client with no token crash `cheesedb serve` or use up its memory. cheesedb now needs Go 1.26.11 or newer (`go.mod`), and the Docker image is built with it. Everyone running `cheesedb serve` should update.
- **Security: a memory with very many legs is checked quickly.** Finding identical legs compared every leg with every other one, so one request with 200,000 legs held a CPU for about 15 seconds, and opening the store paid that again. The check now takes one pass.
- **Security: one agent cannot take every request slot.** A token holder could hold all 256 slots by sending request bodies slowly. Each agent may now run at most 32 requests at once (`MaxPerAgent`, by default an eighth of `MaxRequests`); beyond that it gets `429 agent_busy` with `Retry-After`, and other agents still get through.
- **Security: the `--tls-auto` certificate is limited to its own names.** It is its own certificate authority, so `tls.key` could have signed a certificate for any website, and a machine that trusts `tls.crt` would have believed it. It now carries name constraints (only localhost, 127.0.0.1, ::1 and the `--tls-host` names) and may sign no further authorities. A certificate made by an earlier version keeps working but is not limited: delete `tls.crt` and `tls.key` to make a new one.
- **Security: CI and image hardening.** Every GitHub Action is pinned to a commit, the release-results workflow accepts only `vX.Y.Z` tags and no longer pastes step output into a shell line, and the Docker base images are pinned by digest.
- **Security: the image is scanned and signed.** The image now runs on distroless Debian 13 (the Debian 12 base carried four pending security updates; Debian 13 has none). Each release's image is scanned with Trivy before it is pushed, and a fixable HIGH or CRITICAL vulnerability stops the release. The pushed image is signed with cosign (keyless, through GitHub); `.github/workflows/image.yml` shows the `cosign verify` command.

## v0.4.0 (preview)

- **A `llm.conf` that holds a key must be private.** When the file holds `api_key` or `embed_api_key` and other users can read it, the LLM and search commands refuse to run and print the `chmod 600` line that fixes it, as `ssh` does for a private key. The mode check is Unix-only; on Windows the permission bits do not say who can read a file. A file with no key can stay readable.
- **The raw benchmark output is kept in the repository** ([`bench/results/v0.3.0/`](bench/results/v0.3.0/)): every benchmark command's unedited output, with the commit, machine, versions and test program checksums. BENCHMARKS.md, the README and the website now come from that run. Most numbers moved by a few percent; the largest change is PostgreSQL's read by id (44 to 59 µs). cheesedb's in-memory recall walk measured slower than in the earlier run (169 µs instead of 109 µs for 10,000 memories), which BENCHMARKS.md says.
- **`bench/run-all.sh`** repeats the whole benchmark run with one command.
- **Every release gets its test results:** when a release is published, the full test suite runs on Linux, macOS and Windows and each output is attached to the release.
- **`cheesedb serve` (preview, not for use yet):** the store over HTTP + JSON on 127.0.0.1, or a Unix socket, so programs in any language can use it and an agent's log and indexes stay loaded between calls. The token in the `Authorization` header picks the agent; no URL or field names one. Requests from web pages are refused, as are other addresses without TLS. Shutdown lets running requests finish. Sessions approved by a person are still to come, and the server should not be used before they are.
- **A Docker image** (`ghcr.io/govinargo-labs/cheesedb:0.4.0`, built from `Dockerfile`): the server in a 17 MB image with no shell, as a non-root user, keeping everything in `/data`. It serves TLS only: `cheesedb serve --tls-auto <folder>` makes a self-signed certificate there on first start and reuses it after.
- **Opening a large agent no longer holds up other agents.** The first open of an agent replays its log, which takes seconds for a large one. Other agents' opens, closes and creates used to wait for it; now only opens of that same agent wait, and they share its result.

## v0.3.0 (preview)

- **Vector search (preview).** An agent can keep vector indexes and find lots and memories by meaning: `CreateIndex`, `Embed`, `EmbedBatch`, `Search`, `Indexes`, `VerifyIndex`, `DropIndex`. You supply the embeddings; cheesedb never calls a model. Search is exact (cosine similarity). A recall removes the vectors of everything it removes, at once, and `Erase` and `Compact` remove them from disk. Each index has its own folder, log and hash chain; an agent without an index writes the same files and records as before. If an interrupted erase cannot finish because an index is damaged, the agent still opens and `Report()` says so (`UnfinishedErase`, `EraseErr`). The index format may still change. See [Ask in plain English](README.md#ask-in-plain-english) and [ARCHITECTURE.md section 9](ARCHITECTURE.md#9-vector-indexes).
- **Search from the command line and MCP apps.** With an embedding model in `llm.conf` (`embed_model`, optionally `embed_base_url` and `embed_api_key`), `remember` also stores each memory's vector, MCP apps get a `search` tool, and the command gets `cheesedb search`, `cheesedb embed` (make existing memories searchable, or rebuild after a model change) and `cheesedb index`. `cheesedb ask` now searches first and gives the chat model only the closest live memories (10 by default), instead of every live memory, so it still works when an agent has thousands; it needs the embedding model too. `cheesedb forget <agent> "<what>"` finds the memory closest in meaning, shows it and every memory that would go with it, and erases them only after a yes (memories only: a source is still recalled by its number).
- **`WouldRecall`** previews what a `Recall` would remove, without writing anything, so a program can ask a person before a permanent recall.
- **The MCP `recall` tool takes two calls.** The first removes nothing and lists what would be removed, with a confirm code that fits only that list; the second, with the code, recalls. Apps that support MCP elicitation also ask you directly, in a form the model cannot fill in. Elsewhere it is a speed bump: use the app's tool approval for `recall`.
- **`cheesedb learn` makes what it learns searchable** when an embedding model is set up, so another app sharing the same memory (for example Claude Code over MCP) finds it.
- **`cheesedb find`** also searches inside an agent's index folders, instead of failing on them.
- **A runnable demo:** `go run ./examples/vector-search`, offline, or with a real embedding model from any OpenAI-compatible server.
- **`examples/plain-english`:** the peanut story step by step from the command line (search, ask, recall, erase, find, verify), for a demo or a screen recording.
- **Search indexes take a quarter of the RAM:** vectors are held at one byte per value (int8), and the few rows that could make the top results are re-checked with the exact float32 values on disk, so results are unchanged. At 1M memories: 1.1 GB instead of 3.6 GB.
- **Measured from 1,000 to 1M memories** ([BENCHMARKS.md](BENCHMARKS.md#search-index-cheesedbs-own-cost-by-size)): search takes 1.1 ms at 1,000 memories and 142 ms at 1M from Go with the store open; the command and MCP server reload the index for every call (10 ms at 1,000, 7.5 s at 1M).
- **Website:** [cheesedb.dev](https://cheesedb.dev) now has a step-by-step recall game and pages for MCP, numbers and limits. Its files no longer live in this repository. The architecture picture is drawn from `docs/diagram/architecture.html` and now shows search.
- **A golden store written by v0.2.0** is kept in `testdata/`: every later version must open it with the same head fingerprint and answers, and write the same records.
- **Closing an agent no longer holds the store's lock** while the agent finishes a write in progress, so other agents can be opened and closed meanwhile.

## v0.2.0 (preview)

- **A local MCP server: `cheesedb mcp <agent>`.** Claude Code, Cursor and other MCP apps can use one agent's memory with no code, through five tools (`add_lot`, `remember`, `recall`, `get`, `list_memories`), over standard input and output. No tool takes an agent id. `recall` erases the text from disk unless told otherwise, and `get` never shows the text of a recalled item. A model that can run shell commands can still bypass the tools; see [Use from AI apps](README.md#use-from-ai-apps-mcp) and [THREAT_MODEL.md](THREAT_MODEL.md).
- **CI runs `staticcheck` and `govulncheck`** in a separate `lint` job, and the workflow's token is read-only.
- **Windows:** `Open` no longer fails on the directory flush, which Windows rejects. The guarantee there is weaker than on macOS and Linux (see [Limits](README.md#limits)); Windows support is still not complete.

## v0.1.1 (preview)

- **`cheesedb version`** prints the version of the command, for example `cheesedb v0.1.0`.
- **A fuzz test for the record decoder** (`FuzzDecode`): `Decode` must never panic on any bytes, every failure must be a documented error, and anything it accepts must encode back to the same bytes. Run it with `go test -fuzz=FuzzDecode ./internal/record`.
- **Benchmarks now include Neo4j** (a graph database), and the 10,000-memory recall is measured end to end instead of computed. See [BENCHMARKS.md](BENCHMARKS.md).
- **The website is now at [cheesedb.dev](https://cheesedb.dev).**

## v0.1.0 (preview)

First public preview. Not yet independently reviewed, and not yet recommended for production data (see [Limits](README.md#limits)).

- **Memory model:** lots (sources), memories with legs, Fair and Strict recall of a lot or a memory, `Get`, `Active`.
- **Erasure:** `Recall(..., Erase)` removes the text of the recalled record and of every dead record below it from disk; `Compact()` erases all dead text.
- **Storage:** append-only log in numbered segments with seal files; group commit with fsync; torn-tail recovery; a hash chain checked on every open; `Verify()` returns the head fingerprint.
- **Isolation:** one store folder of agents, each reachable only with its token; a file lock keeps a second process out.
- **The `cheesedb` command:** every feature from the command line, plus an LLM demo comparing a plain memory list, `Untrusted` and `Erase`.
- **Benchmarks** against SQLite and PostgreSQL ([BENCHMARKS.md](BENCHMARKS.md)).

Known gaps: Windows is not supported yet (tests run in CI to track it); no fuzzing or crash-injection tests yet. Developed on macOS; CI also runs the tests on Linux.
