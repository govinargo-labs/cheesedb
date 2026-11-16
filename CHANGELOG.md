# Changelog

cheesedb follows [semantic versioning](https://semver.org). Until v1.0.0, the API and the on-disk format may change between minor versions.

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
- **Website:** split into pages (home with a step-by-step recall game, MCP, numbers, limits) with shared style and script files; the architecture picture is drawn from `docs/diagram/architecture.html` and now shows search.
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
