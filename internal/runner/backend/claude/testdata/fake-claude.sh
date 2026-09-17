#!/bin/sh
# Stand-in for `claude` in tests. Replays a recorded stream and can misbehave on request.
#   FAKE_CLAUDE_FIXTURE  file to print on stdout (required)
#   FAKE_CLAUDE_ARGS     append the argument list here, one per line, then "---"
#   FAKE_CLAUDE_COMMIT   make a commit in the working directory first
#   FAKE_CLAUDE_CHILD    start a background process that inherits stdout; write its pid here
#   FAKE_CLAUDE_SLEEP    sleep this many seconds after printing
#   FAKE_CLAUDE_EXIT     exit code (default 0)
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
cat "$FAKE_CLAUDE_FIXTURE"
if [ -n "$FAKE_CLAUDE_SLEEP" ]; then sleep "$FAKE_CLAUDE_SLEEP"; fi
exit "${FAKE_CLAUDE_EXIT:-0}"
