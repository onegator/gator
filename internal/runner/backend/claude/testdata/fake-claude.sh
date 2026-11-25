#!/bin/sh
# Stand-in for `claude` in tests. Replays a recorded stream and can misbehave on request.
#   FAKE_CLAUDE_FIXTURE  file to print on stdout (required)
#   FAKE_CLAUDE_ARGS     append the argument list here, one per line, then "---"
#   FAKE_CLAUDE_COMMIT   make a commit in the working directory first
#   FAKE_CLAUDE_CHILD    start a background process that inherits stdout; write its pid here
#   FAKE_CLAUDE_SLEEP    sleep this many seconds after printing
#   FAKE_CLAUDE_EXIT     exit code (default 0)
#   FAKE_CLAUDE_ENV      write the environment it was given here
#   FAKE_CLAUDE_READ     try to read this file; write what happened to FAKE_CLAUDE_READ.out
#   FAKE_CLAUDE_MCP      copy the file --mcp-config names here (or the JSON itself), with its mode
#   FAKE_CLAUDE_SUBST    a sed script applied to the fixture (an id or a number known only at run time)
#   FAKE_CLAUDE_PLUGIN   list the files of the directory --plugin-dir names here, with their modes
#   FAKE_CLAUDE_DUMP     write everything this run can see of the job here: its environment, its arguments, and
#                        every file of the working directory and of the runner's directories it was pointed at
if [ -n "$FAKE_CLAUDE_ENV" ]; then env > "$FAKE_CLAUDE_ENV"; fi
if [ -n "$FAKE_CLAUDE_READ" ]; then
	if cat "$FAKE_CLAUDE_READ" > "$FAKE_CLAUDE_READ.out" 2>&1; then echo "read" >> "$FAKE_CLAUDE_READ.out"; else echo "denied" >> "$FAKE_CLAUDE_READ.out"; fi
fi
if [ -n "$FAKE_CLAUDE_MCP" ]; then
	prev=""
	for a in "$@"; do
		if [ "$prev" = "--mcp-config" ]; then
			if [ -f "$a" ]; then cat "$a" > "$FAKE_CLAUDE_MCP"; stat -c '%a' "$a" > "$FAKE_CLAUDE_MCP.mode" 2>/dev/null || stat -f '%Lp' "$a" > "$FAKE_CLAUDE_MCP.mode"; echo "$a" > "$FAKE_CLAUDE_MCP.path"
			else printf '%s' "$a" > "$FAKE_CLAUDE_MCP"; fi
		fi
		prev="$a"
	done
fi
if [ -n "$FAKE_CLAUDE_PLUGIN" ]; then
	prev=""
	for a in "$@"; do
		if [ "$prev" = "--plugin-dir" ]; then
			(cd "$a" && find . -type f | sort | while read -r f; do printf '%s %s\n' "$(stat -c '%a' "$f" 2>/dev/null || stat -f '%Lp' "$f")" "$f"; done) > "$FAKE_CLAUDE_PLUGIN"
		fi
		prev="$a"
	done
fi
if [ -n "$FAKE_CLAUDE_DUMP" ]; then
	{
		env
		printf '%s\n' "$@"
		prev=""
		for a in "$@"; do
			if [ "$prev" = "--mcp-config" ] && [ -f "$a" ]; then find "$(dirname "$a")" -type f -exec cat {} + 2>/dev/null; fi
			if [ "$prev" = "--plugin-dir" ]; then find "$a" -type f -exec cat {} + 2>/dev/null; fi
			prev="$a"
		done
		find . -type f -not -path './.git/*' -exec cat {} + 2>/dev/null
		find . -path './.git/*' -type f -name config -exec cat {} + 2>/dev/null
	} > "$FAKE_CLAUDE_DUMP"
fi
if [ -n "$FAKE_CLAUDE_ARGS" ]; then
	for a in "$@"; do printf '%s\n' "$a" >> "$FAKE_CLAUDE_ARGS"; done
	echo "---" >> "$FAKE_CLAUDE_ARGS"
fi
#   FAKE_CLAUDE_RESUME_FIXTURE  printed instead (and nothing committed) when --resume is passed
for a in "$@"; do
	if [ "$a" = "--resume" ] && [ -n "$FAKE_CLAUDE_RESUME_FIXTURE" ]; then
		cat "$FAKE_CLAUDE_RESUME_FIXTURE"
		exit 0
	fi
done
if [ -n "$FAKE_CLAUDE_COMMIT" ]; then
	echo "change $$" >> gator.txt
	git add gator.txt && git -c user.name=fake -c user.email=fake@t -c commit.gpgsign=false commit -q -m "fake: change"
fi
if [ -n "$FAKE_CLAUDE_CHILD" ]; then
	sleep 300 &
	echo $! > "$FAKE_CLAUDE_CHILD"
fi
if [ -n "$FAKE_CLAUDE_SUBST" ]; then sed -e "$FAKE_CLAUDE_SUBST" "$FAKE_CLAUDE_FIXTURE"; else cat "$FAKE_CLAUDE_FIXTURE"; fi
if [ -n "$FAKE_CLAUDE_SLEEP" ]; then sleep "$FAKE_CLAUDE_SLEEP"; fi
exit "${FAKE_CLAUDE_EXIT:-0}"
