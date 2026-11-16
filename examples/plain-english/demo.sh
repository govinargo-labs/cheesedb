#!/usr/bin/env bash
# The peanut story: plain-English search, ask, recall and erase, step by step.
#
# Needs: the cheesedb command on your PATH, and in ~/.cheesedb/llm.conf an
# embedding model (embed_model = ...) for search, and a chat model
# (model = ...) for ask. See README.md next to this file.
# Press Enter to go to the next step; PAUSE=0 runs it without stopping.
#
# It deletes and recreates the agent "story" each time it starts.

A=story

say()   { printf '\n\033[1;36m# %s\033[0m\n' "$*"; }
note()  { printf '\033[2m# %s\033[0m\n' "$*"; }
pause() { [ "${PAUSE:-1}" = 0 ] || read -rp $'\n\033[1;33m>> press Enter for the next step\033[0m ' _; }
run() {
	local shown="" arg
	for arg in "$@"; do
		case $arg in *" "*|*"?"*|*"'"*) shown+=" \"$arg\"" ;; *) shown+=" $arg" ;; esac
	done
	printf '\033[1m$ cheesedb%s\033[0m\n' "$shown"
	cheesedb "$@"
}

clear
printf '\033[1mcheesedb: memory for AI agents that knows where every fact came from\033[0m\n'
printf 'Setting up a fresh agent "%s"...\n' "$A"
cheesedb agent delete $A >/dev/null 2>&1
cheesedb agent create $A >/dev/null || exit 1
pause

say "1. The assistant hears three things (sources)"
run lot $A user_message "I am allergic to peanuts"
run lot $A doctor_note "Allergy test result: peanut allergy confirmed"
run lot $A web_page "Spice Garden menu: our satay sauce is peanut-free"
pause

say "2. It learns facts, and each fact records its sources"
note "from the user OR the doctor (1 2)"
run remember $A "The user is allergic to peanuts" 1 2
note "from the web page (3)"
run remember $A "Spice Garden's satay sauce has no peanuts" 3
note "needs BOTH facts above (4,5)"
run remember $A "The satay at Spice Garden is safe for the user" 4,5
pause

say "3. Search memory by meaning"
run search $A "can the user eat the satay at Spice Garden?"
pause

say "4. Ask the AI: it answers from those memories only"
run ask $A "Can the user eat the satay at Spice Garden?"
pause

say "5. The web page was wrong. Recall it, and erase its text"
run recall $A 3 fair erase
pause

say "6. Everything learned from it is gone"
run search $A "can the user eat the satay at Spice Garden?"
run ask $A "Can the user eat the satay at Spice Garden?"
pause

say "7. Gone from the disk too, not just hidden"
run find $A "peanut-free"
pause

say "8. True facts survive: drop the user's message, the doctor's note still backs the allergy"
run recall $A 1 fair erase
run active $A
run ask $A "Can the user eat peanuts?"
pause

say "9. The whole history is hash-chained and checkable"
run verify $A

printf '\n\033[1mcheesedb: every memory knows its sources. One recall removes a bad source and\neverything built on it, from search, from the AI, and from the disk.\033[0m\n\n'
