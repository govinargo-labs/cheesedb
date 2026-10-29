package main

// The LLM side of the cheesedb command: an "agent" that learns memories from its
// sources and answers questions from its live memories. cheesedb only
// stores what the agent declares; the legs here come from the LLM, so they
// are exactly as good as the model's citations (README Limits: "legs are
// declared by the caller").

import (
	"fmt"
	"strings"

	"github.com/govinargo-labs/cheesedb"
)

const learnFactsPrompt = `You turn sources into short facts an assistant should remember about the user and their situation. Use only what the sources say.
Reply with JSON only, in this shape:
{"facts": [{"text": "...", "sources": [[1], [2]]}]}
"sources" lists the ways the fact is supported. Each inner list is one way: the ids of the sources that are ALL needed for it.
If two sources each support the fact on their own, give two lists: [[1], [2]]. If the fact needs two sources together, give one list: [[1, 3]].`

const learnConclusionsPrompt = `You draw practical conclusions an assistant could act on, by combining the remembered facts below. Use only these facts.
Reply with JSON only, in this shape:
{"facts": [{"text": "...", "sources": [[4, 5]]}]}
Each inner list holds the ids of the facts that are ALL needed for the conclusion. If nothing useful follows, reply {"facts": []}.`

const askPrompt = `You are a personal assistant. Answer the question using ONLY the memories listed: you know nothing else about the user or the world, including weather or places.
If the memories are not enough to answer, say so briefly. Answer in one or two sentences.`

type learned struct {
	Facts []struct {
		Text    string     `json:"text"`
		Sources [][]uint64 `json:"sources"`
	} `json:"facts"`
}

// fact is one memory the LLM proposed, and what cheesedb did with it.
type fact struct {
	pass int // 1: from sources; 2: a conclusion from memories
	id   cheesedb.ID
	text string
	legs []cheesedb.Leg
	err  error // set when cheesedb refused it
}

// learn runs two LLM passes: facts from the live sources, then conclusions
// from the live memories. Each result is stored with the legs the LLM gave.
// cheesedb checks them: an invented or recalled id is refused. learn prints
// nothing; `cheesedb learn` and the demo each show the result their own way.
func learn(c *llm, a *cheesedb.Agent) ([]fact, error) {
	var sources strings.Builder
	for id := cheesedb.ID(1); id <= cheesedb.ID(a.Report().Records); id++ {
		if it, err := a.Get(id); err == nil && it.Kind == cheesedb.KindLot && it.Alive && !it.Redacted {
			fmt.Fprintf(&sources, "[%d] %s\n", id, it.Data)
		}
	}
	if sources.Len() == 0 {
		return nil, fmt.Errorf("no live sources to learn from; add some with: cheesedb lot")
	}

	var facts learned
	if err := c.chatJSON(learnFactsPrompt, "Sources:\n"+sources.String(), &facts); err != nil {
		return nil, err
	}
	out := remember(a, 1, facts)

	// Pass 2 sees the memories pass 1 just stored.
	var mems strings.Builder
	ids, err := a.Active()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		it, err := a.Get(id)
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&mems, "[%d] %s\n", id, it.Data)
	}
	var conclusions learned
	if err := c.chatJSON(learnConclusionsPrompt, "Facts:\n"+mems.String(), &conclusions); err != nil {
		return nil, err
	}
	return append(out, remember(a, 2, conclusions)...), nil
}

func remember(a *cheesedb.Agent, pass int, l learned) []fact {
	out := make([]fact, 0, len(l.Facts))
	for _, f := range l.Facts {
		legs := make([]cheesedb.Leg, 0, len(f.Sources))
		for _, src := range f.Sources {
			leg := make(cheesedb.Leg, len(src))
			for i, id := range src {
				leg[i] = cheesedb.ID(id)
			}
			legs = append(legs, leg)
		}
		id, err := a.Remember([]byte(f.Text), legs...)
		out = append(out, fact{pass, id, f.Text, legs, err})
	}
	return out
}

// printFacts shows what learn stored, for `cheesedb learn`.
func printFacts(facts []fact) {
	pass, n := 0, 0
	for _, f := range facts {
		if f.pass != pass {
			pass = f.pass
			fmt.Println([]string{"", "  pass 1: facts from sources", "  pass 2: conclusions from memories"}[pass])
		}
		if f.err != nil {
			fmt.Printf("    refused by cheesedb (%v): %q\n", f.err, f.text)
			continue
		}
		fmt.Printf("    [%d] %q  ← learned from %s\n", f.id, f.text, describeLegs(f.legs))
		n++
	}
	fmt.Printf("  stored %d memories\n", n)
}

// describeLegs writes legs the way we say them: "[1] or [2]", "[4 and 5]".
func describeLegs(legs []cheesedb.Leg) string {
	parts := make([]string, len(legs))
	for i, leg := range legs {
		ids := make([]string, len(leg))
		for j, id := range leg {
			ids[j] = fmt.Sprint(id)
		}
		parts[i] = "[" + strings.Join(ids, " and ") + "]"
	}
	return strings.Join(parts, " or ")
}

// liveMemoryTexts is everything the agent may use: its live memories.
func liveMemoryTexts(a *cheesedb.Agent) ([]string, error) {
	ids, err := a.Active()
	if err != nil {
		return nil, err
	}
	texts := make([]string, 0, len(ids))
	for _, id := range ids {
		it, err := a.Get(id)
		if err != nil {
			return nil, err
		}
		texts = append(texts, string(it.Data))
	}
	return texts, nil
}

// ask gives the LLM only the memories passed in, then the question.
func ask(c *llm, memories []string, question string) (string, error) {
	var b strings.Builder
	if len(memories) == 0 {
		b.WriteString("(no memories)\n")
	}
	for _, m := range memories {
		b.WriteString("- " + m + "\n")
	}
	return c.chat(askPrompt, "Memories:\n"+b.String()+"\nQuestion: "+question)
}
