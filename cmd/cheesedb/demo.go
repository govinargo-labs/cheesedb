package main

// The demo: one story told with three kinds of memory, so you can see what
// "deleting" really means for an agent.
//
//   - A naive store keeps a plain list of memories. Deleting a source
//     removes that source and nothing else; everything learned from it stays,
//     and the model keeps using it.
//   - cheesedb with Recall(..., Untrusted): every memory built on the source
//     dies, so the model no longer sees it, but the text is still on disk.
//   - cheesedb with Recall(..., Erase): the same, and the text is erased
//     from the files. `find` searches the raw bytes to prove it.
//
// The output is laid out to fit one screen (about 100 columns).

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/govinargo-labs/cheesedb"
)

func demo() error {
	c, err := newLLM()
	if err != nil {
		return err
	}
	if err := makeHome(); err != nil {
		return err
	}
	if err := os.RemoveAll(demoDir); err != nil {
		return err
	}
	s, err := cheesedb.Open(demoDir)
	if err != nil {
		return err
	}
	defer s.Close()
	tok, err := s.CreateAgent("demo")
	if err != nil {
		return err
	}
	a, err := s.OpenAgent(tok)
	if err != nil {
		return err
	}
	defer a.Close()

	fmt.Println()
	box("An AI agent learns from a source that turns out to be wrong.",
		"You delete the source. Does the agent forget what it learned?")
	fmt.Println(dim(fmt.Sprintf("  cheesedb demo · real LLM: %s · fresh store in %s", c.model, tilde(demoDir))))

	step(1, "Three sources arrive")
	sources := []struct{ typ, text string }{
		{"user_message", "I live in Pune."},
		{"web_page", "Profile page: Asha is a software engineer based in Pune, India."},
		{"weather_tool", "Weather forecast for Pune today: heavy rain all afternoon."},
	}
	var weather cheesedb.ID
	for _, src := range sources {
		id, err := a.AddLot(src.typ, []byte(src.text))
		if err != nil {
			return err
		}
		fmt.Printf("    %s %s %s\n", dim(fmt.Sprintf("[%d]", id)), cyan(pad(src.typ, 13)), src.text)
		if src.typ == "weather_tool" {
			weather = id
		}
	}

	step(2, "The LLM learns memories and cites where each came from")
	// learn lists sources by Report().Records, which was 0 when this agent
	// was opened; reopen so it sees the three lots.
	a.Close()
	if a, err = s.OpenAgent(tok); err != nil {
		return err
	}
	facts, err := learn(c, a)
	if err != nil {
		return err
	}
	for _, f := range facts {
		if f.err != nil {
			fmt.Printf("    %s %s\n", red("refused by cheesedb:"), clip(f.text, 70))
			continue
		}
		fmt.Printf("    %s %s %s\n", dim(fmt.Sprintf("[%d]", f.id)), pad(clip(f.text, 66), 66),
			dim("← "+describeLegs(f.legs)))
	}

	question := "I'm heading out this afternoon. Should I take an umbrella?"
	step(3, "Ask: "+question)
	before, err := liveMemoryTexts(a)
	if err != nil {
		return err
	}
	if err := answer(c, before, question, fmt.Sprintf("model sees %d memories", len(before))); err != nil {
		return err
	}
	naive := slices.Clone(before) // the naive store holds the same memories

	step(4, fmt.Sprintf("Plot twist: the weather tool was wrong. Remove source [%d], three ways", weather))

	label("A", "Plain memory list", "deletes the source, nothing else")
	if err := answer(c, naive, question,
		fmt.Sprintf("model still sees %d memories: the source is gone, what it taught is not", len(naive))); err != nil {
		return err
	}

	label("B", "cheesedb", fmt.Sprintf("Recall(%d, Fair, Untrusted)", weather))
	removed, err := a.Recall(weather, cheesedb.Fair, cheesedb.Untrusted)
	if err != nil {
		return err
	}
	after, err := liveMemoryTexts(a)
	if err != nil {
		return err
	}
	if err := answer(c, after, question,
		fmt.Sprintf("also removes %s, learned from it · model sees %d memories", ids(removed), len(after))); err != nil {
		return err
	}
	untrusted, err := findHits(demoDir, "demo", "rain")
	if err != nil {
		return err
	}
	onDisk(untrusted)

	label("C", "cheesedb", fmt.Sprintf("Recall(%d, Fair, Erase)", weather))
	if _, err := a.Recall(weather, cheesedb.Fair, cheesedb.Erase); err != nil {
		return err
	}
	fmt.Println("       " + dim(fmt.Sprintf("model sees the same %d memories as B, and the text is erased from the files", len(after))))
	erased, err := findHits(demoDir, "demo", "rain")
	if err != nil {
		return err
	}
	onDisk(erased)

	head, err := a.Verify()
	if err != nil {
		return err
	}
	step(5, "Result")
	no, yes := func(s string) cell { return cell{"✓ " + s, green} }, func(s string) cell { return cell{"✗ " + s, red} }
	untrustedDisk := cell{"✓ no", green}
	if len(untrusted) > 0 {
		untrustedDisk = cell{fmt.Sprintf("! yes, %d places (until Compact)", len(untrusted)), yellow}
	}
	eraseDisk := no("no")
	if len(erased) > 0 {
		eraseDisk = yes(fmt.Sprintf("yes, %d places", len(erased)))
	}
	table([]int{20, 30, 32}, [][]cell{
		{{"", plain}, {"Model still uses the bad data?", bold}, {"Bad text still in the files?", bold}},
		{{"Plain memory list", plain}, yes(fmt.Sprintf("yes, %d memories", len(removed))), yes("yes, in its memories")},
		{{"cheesedb, Untrusted", plain}, no("no"), untrustedDisk},
		{{"cheesedb, Erase", plain}, no("no"), eraseDisk},
	})
	fmt.Println("    " + green("✓") + dim(fmt.Sprintf(" hash chain verified after erasing · head %x…", head[:8])))

	fmt.Println()
	fmt.Println("  " + bold("Every memory records where it came from. Recall a source, and everything built"))
	fmt.Println("  " + bold("on it goes too: from the model's view, and with Erase, from the disk."))
	fmt.Println("  " + dim("cheesedb · append-only, hash-chained memory for AI agents · Go, standard library only"))
	fmt.Println("  " + dim("(the LLM chose the legs; cheesedb removes what it was told came from the weather)"))
	fmt.Println()
	return nil
}

func step(n int, title string) {
	fmt.Printf("\n  %s  %s\n", bold(cyan(fmt.Sprint(n))), bold(title))
}

func label(letter, who, what string) {
	fmt.Printf("\n    %s  %s  %s\n", bold(cyan(letter)), bold(who), dim(what))
}

// answer asks the model with only these memories and prints its reply,
// wrapped under a short note about what the model was given.
func answer(c *llm, memories []string, question, note string) error {
	fmt.Println("       " + dim(note))
	reply, err := ask(c, memories, question)
	if err != nil {
		return err
	}
	for i, line := range wrap(reply, 80) {
		if i == 0 {
			fmt.Println("       " + bold("model ›") + " " + line)
		} else {
			fmt.Println("               " + line)
		}
	}
	return nil
}

// onDisk prints whether "rain" is still in the agent's raw files.
func onDisk(hits []hit) {
	if len(hits) == 0 {
		fmt.Println("       " + green(`✓ on disk: "rain" not found in any file (searched byte by byte)`))
		return
	}
	fmt.Println("       " + yellow(fmt.Sprintf(`! on disk: "rain" still found in %d places`, len(hits))))
	for _, h := range hits {
		fmt.Println("         " + dim(fmt.Sprintf("%s byte %-5d …%s…", h.file, h.at, h.context)))
	}
}

// ids writes [6 7] as "[6] [7]".
func ids(list []cheesedb.ID) string {
	parts := make([]string, len(list))
	for i, id := range list {
		parts[i] = fmt.Sprintf("[%d]", id)
	}
	return strings.Join(parts, " ")
}

// tilde shows a path under the home folder as ~/..., to keep lines short.
func tilde(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h+string(os.PathSeparator)) {
		return "~" + p[len(h):]
	}
	return p
}
