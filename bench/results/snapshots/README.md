# Benchmark run: snapshots (unreleased)

A new run, separate from [v0.3.0](../v0.3.0/), made for the snapshot change. It measures only opening an agent (with and without `state.snap`) and writing a snapshot, at 10k, 100k and 1M memories. The raw output is unedited. Each number in BENCHMARKS.md is the median of the three runs here.

| File | What it ran |
|---|---|
| `env.txt` | commit, machine, OS and Go version |
| `cheesedb-open.txt` | `BenchmarkOpen` (full replay), `BenchmarkOpenSnapshot` (from a snapshot covering the whole log) and `BenchmarkSnapshotWrite` (what `Close` pays when it writes one) |
