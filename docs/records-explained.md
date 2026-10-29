# cheesedb records, explained by example

## How a record is built

Every record in cheesedb has three parts, and only the last one can ever be erased. This walkthrough follows one agent's log from its first record to its eighth, using a single running example.

| Part | What it holds | Can it be erased? |
| --- | --- | --- |
| Header | The record's number, and a seal linking it to the record before it | No |
| Structure | What kind of record it is, and what it depends on | No |
| Content | The actual text, such as a memory or a source's details | Yes, on an erase |

Records are only ever added to the end of the log. Nothing is edited in place, and even a recall is written as a new record.

## Records 1–3: sources arrive

Before an agent can learn anything, each source it uses is registered as a lot. Here the agent receives three: something the user said, a web page and a weather tool.

**Record 1: the user says something**

```text
#1  LOT
  header:     id 1 · sealed to: start of log
  structure:  type = user_message
  content:    "I live in Pune"
```

**Record 2: a web page the agent read**

```text
#2  LOT
  header:     id 2 · sealed to: record 1
  structure:  type = web_page
  content:    "example.com/pune-weather"
```

**Record 3: a weather tool's answer**

```text
#3  LOT
  header:     id 3 · sealed to: record 2
  structure:  type = tool
  content:    "weather_api: rain expected"
```

Each header is sealed to the record before it. Changing or removing any record breaks that chain, so tampering is detected.

## Records 4–6: memories with legs

A memory's structure lists its legs: each leg is a group of parent records that must all stay alive. Inside a leg it's AND; between legs it's OR, so any one leg keeps the memory alive.

**Record 4: learned two independent ways**

```text
#4  MEMORY
  header:     id 4 · sealed to: record 3
  structure:  legs = [ [1], [2] ]
  content:    "User lives in Pune"
```

The user said it (record 1), and the web page also showed it (record 2). Either one alone keeps this memory alive.

**Record 5: learned from the weather tool**

```text
#5  MEMORY
  header:     id 5 · sealed to: record 4
  structure:  legs = [ [3] ]
  content:    "Pune is rainy this week"
```

One leg with one parent: if record 3 goes, so does this memory.

**Record 6: a conclusion built from two memories together**

```text
#6  MEMORY
  header:     id 6 · sealed to: record 5
  structure:  legs = [ [4, 5] ]
  content:    "Remind user to carry an umbrella"
```

One leg with two parents: it needs both memory 4 and memory 5. If either dies, this memory dies too.

## Record 7: the weather tool turns out to be wrong

Recalling an untrusted source kills everything that depended only on it, and nothing is removed from disk.

```text
#7  RECALL
  header:     id 7 · sealed to: record 6
  structure:  target = 3 · rule = fair · reason = untrusted
  content:    (none)
```

What happens:

- Memory 5 had only one leg, the weather tool, so it dies.
- Memory 6 needed memory 5, so it dies too.
- Memory 4 doesn't depend on the tool, so it's untouched.
- The dead memories are hidden from the agent, but their text stays on disk.

## Record 8: the web page must be erased

An erase physically removes a record's text from disk but keeps its header and structure, so the history stays complete. Here the web page turns out to hold someone else's personal data.

```text
#8  RECALL
  header:     id 8 · sealed to: record 7
  structure:  target = 2 · rule = fair · reason = erase
  content:    (none)
```

Record 2 now looks like this on disk:

```text
#2  LOT (erased)
  header:     id 2 · sealed to: record 1
  structure:  type = web_page
  content:    — removed —
```

What happens:

- The web page's text is gone for good.
- The header and structure stay, so the seals still line up and the family tree is still complete.
- Memory 4 loses its web-page leg, but the user also said it (record 1), so it survives with its text.
- Under the strict rule, memory 4 would have been erased too, because the web page touched it at all.

## Final state

After both recalls, only the user's own words and the memory they support remain alive.

| Record | What | Status | Content on disk |
| --- | --- | --- | --- |
| 1 | User said "I live in Pune" | Alive | Kept |
| 2 | Web page | Recalled | Erased |
| 3 | Weather tool | Recalled | Kept (untrusted, not erased) |
| 4 | "User lives in Pune" | Alive | Kept |
| 5 | "Pune is rainy this week" | Dead | Kept, hidden |
| 6 | "Remind user to carry an umbrella" | Dead | Kept, hidden |
| 7 | Recall of record 3 | Recall record | No content |
| 8 | Recall of record 2 | Recall record | No content |

## Four things to notice

1. Records are only ever added. Even a recall is a new record; nothing is edited in place.
2. Each record is sealed to the one before it, so a changed or removed record breaks the chain and is detected.
3. An erase removes words, not history. Record 2's text is gone, but the log still shows that a web page existed, was used and was erased.
4. Fair or strict is a choice. Fair keeps memories that another source still supports; strict removes everything a bad source touched.
