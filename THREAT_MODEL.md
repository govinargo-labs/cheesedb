# cheesedb threat model

Who cheesedb trusts, who it defends against, and what it explicitly does not protect. Written for v1. Each later version adds a column, not a rewrite.

## What is being protected

1. **Integrity of each agent's memory.**
   - Memories follow their sources: no memory survives recall of what it depended on.
   - No memory is built on a recalled source.
   - The history is not edited silently.
2. **Separation between agents.** One agent's handle cannot read or change another agent's data.
3. **Erasure on request** (`Recall(..., Erase)`). The content of the recalled source and of every dead memory below it is removed from the live files before `Recall` returns. `Compact()` does the same for other dead records.

## Who is trusted

| | v1 (embedded library, now) | v2 (memory server, planned) |
|---|---|---|
| **Host program** (the code that calls `cheesedb.Open`) | Fully trusted: it holds every token it creates, and it can read the store folder | Not trusted with other agents' data: it talks to the server over a Unix socket with its own token |
| **OS user running the host** | Fully trusted: owns the folder (0700) and files (0600) | The server runs as its own OS user; hosts cannot read its files |
| **OS root / machine admin** | Trusted (cannot be defended against) | Trusted for availability; the external witness detects their history rewrites |
| **Disk and file system** | Trusted to keep what fsync acknowledged. Accidental damage is detected (crc, digest, chain), not repaired | Same |
| **External witness** | None. The caller may keep `Verify()`'s head fingerprint elsewhere by hand | Receives signed head checkpoints; detects rewrites and truncation |

## Attackers in v1

| Attacker | Can | Cannot |
|---|---|---|
| **Prompt-injected LLM** using a tool built on cheesedb | Write false memories through that agent's handle, if the tool allows it. Recall that agent's memories. | Reach another agent: no method takes an agent name or id, so there is no parameter to inject. Build a memory on a recalled source. Undo a recall. |
| **Buggy tool or plugin** with one agent's handle | The same as the LLM above, by mistake | The same as above |
| **Another agent's code** (holding only its own token) | Everything to its own agent | Open another agent: tokens are 32 random bytes, and the store keeps only their SHA-256, so the files do not reveal tokens. A name is not a token. |
| **Someone who reads the store folder** (backup, stolen disk, same OS user) | Read every memory's content (not encrypted in v1). Read the agent names (hex is an encoding, not encryption). | Learn any token. Learn erased content from the current files: the content is gone, and its salted digest reveals nothing. (Older copies of the files may still hold it.) |
| **Someone who writes the store folder** (same OS user, root, compromised host) | Corrupt or delete anything. Redact records. Rewrite the history if they recompute every hash after their change; remove the newest records. | Do it unnoticed if a head fingerprint was kept elsewhere and is compared later. Edit any record in the middle without rebuilding the chain (`Open` and `Verify` refuse it). |
| **A crash or power loss** | Lose writes that were never acknowledged | Lose acknowledged writes. Leave a half-written record, seal or agent folder that `Open` does not detect and handle. |

## Explicitly not protected (v1)

- **Confidentiality at rest.** Content is stored unencrypted. A `keyID` field is reserved for per-agent encryption.
- **The host program.** It can read every agent. Isolation by token protects agents from tools and from each other, not from the program that owns the store.
- **Deliberate history rewrites by someone with write access,** unless the caller keeps head fingerprints elsewhere. v2: signatures and a witness.
- **Secure deletion.** `DeleteAgent` and redaction remove files and content. Backups, snapshots and SSD blocks may keep copies.
- **Two processes on one store, where the lock cannot work.** The file lock is advisory: it stops a second cheesedb, not a program that ignores it. It may not work on network file systems, and some systems (Solaris, AIX) have none.
- **Denial of service.** An agent can fill the disk, or make its own log slow to replay. Quotas are not built.
- **Truth of the legs.** Callers declare legs. cheesedb cascades over what is declared, not over what an agent "really" read. (v2: recording reads per session as default legs.)
- **Code execution by agents.** cheesedb stores data; it does not run agents or tools. Agents that execute code need a sandbox per agent (for example wazero, or a process per agent). That is the host's job.

## Mechanisms and what each one is for

| Mechanism | Defends against |
|---|---|
| Token per agent; only SHA-256(token) stored; no method takes an agent name | One agent, tool or injected prompt reaching another agent |
| Hex-encoded folder names; 1-127 byte names | Path tricks (`../x`, `a/b`) and case-insensitive name collisions |
| Folder 0700, files 0600 | Other OS users reading or changing the store |
| CRC-32C per record and per seal | Accidental damage: torn writes, flipped bits |
| Salted content digest | A content edit that keeps the crc valid. After redaction, guessing the erased content. |
| Hash chain checked on every Open and by `Verify` | Editing, removing, inserting or reordering records in the middle of the history |
| Head fingerprint from `Verify` | Rebuilt chains and removed newest records, when a copy is kept elsewhere |
| Seal files | Damage to, or tampering with, sealed segments; a sealed segment being removed |
| Write lock per agent; one ledger per agent in a store | Two writers on one agent's files inside one process |
| File lock on the store folder (`LOCK`), taken first in `cheesedb.Open` | A second process (or a second `Open`) writing the same files, or deleting another process's half-created agent |
| Redaction (`Erase`, `Compact`): segment rewritten without the content, atomic rename, finished on `Open` after a crash | Erased content remaining in the live files |

## Before any real-world use

An external security review (on the roadmap).
