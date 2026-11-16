# The peanut story, in plain English

A user is allergic to peanuts. A restaurant's web page says its satay sauce is peanut-free, so the assistant concludes the satay is safe. The page was wrong. This script shows, step by step, how one recall removes the page and everything learned from it: from search, from the AI's answers, and from the disk, while the allergy stays.

## Run it

1. Install the command: `go install github.com/govinargo-labs/cheesedb/cmd/cheesedb@latest`
2. Start an OpenAI-compatible server with an embedding model and a chat model (Ollama, LM Studio, oMLX, vLLM, or a hosted API).
3. Put both in `~/.cheesedb/llm.conf`:

   ```
   base_url    = http://localhost:11434/v1
   model       = your-chat-model
   embed_model = nomic-embed-text
   ```

4. Run `./demo.sh` and press Enter at each step. `PAUSE=0 ./demo.sh` runs it without stopping.

It creates (and recreates) an agent called `story` in `~/.cheesedb`; set `CHEESEDB_DIR` to use another folder.

## The steps

| Step | Command | What it shows |
|---|---|---|
| 1 | `cheesedb lot` | Three sources: the user, a doctor's note, the restaurant's web page. |
| 2 | `cheesedb remember` | Three memories, each naming its sources: `1 2` means from 1 OR 2, `4,5` means needs 4 AND 5. |
| 3 | `cheesedb search` | Finds memories by meaning: the question shares few words with the answers. |
| 4 | `cheesedb ask` | Search, then the chat model answers from those memories only: "Yes, the user can eat the satay". |
| 5 | `cheesedb recall 3 fair erase` | The web page was wrong: it and the two memories built on it are removed. |
| 6 | `cheesedb search`, `ask` | Only the allergy is left; the model now says it does not know. |
| 7 | `cheesedb find` | The erased text is not anywhere in the files, byte by byte. |
| 8 | `cheesedb recall 1 fair erase` | The user's own message is removed, but the allergy stays: the doctor's note still backs it. |
| 9 | `cheesedb verify` | The history is intact. |

## Without a chat model

Only `ask` needs the chat model. Remove the two `run ask` lines and everything else works with the embedding model alone. Without an embedding model too, use `cheesedb active story` instead of `search`: recall and erase work the same.
