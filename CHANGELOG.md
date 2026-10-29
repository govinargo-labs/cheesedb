# Changelog

cheesedb follows [semantic versioning](https://semver.org). Until v1.0.0, the API and the on-disk format may change between minor versions.

## v0.1.0 (preview)

First public preview. Not yet independently reviewed, and not yet recommended for production data (see [Limits](README.md#limits)).

- **Memory model:** lots (sources), memories with legs, Fair and Strict recall of a lot or a memory, `Get`, `Active`.
- **Erasure:** `Recall(..., Erase)` removes the text of the recalled record and of every dead record below it from disk; `Compact()` erases all dead text.
- **Storage:** append-only log in numbered segments with seal files; group commit with fsync; torn-tail recovery; a hash chain checked on every open; `Verify()` returns the head fingerprint.
- **Isolation:** one store folder of agents, each reachable only with its token; a file lock keeps a second process out.
- **The `cheesedb` command:** every feature from the command line, plus an LLM demo comparing a plain memory list, `Untrusted` and `Erase`.
- **Benchmarks** against SQLite and PostgreSQL ([BENCHMARKS.md](BENCHMARKS.md)).

Known gaps: Windows is not supported yet (tests run in CI to track it); no fuzzing or crash-injection tests yet. Developed on macOS; CI also runs the tests on Linux.
