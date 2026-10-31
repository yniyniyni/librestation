#!/bin/sh
# Fake piper --json-input. Usage: fakepiper.sh WAV STARTLOG [EXIT_AFTER]
# Appends a line to STARTLOG per process start; copies WAV to each
# request's output_file and prints the path; exits after EXIT_AFTER lines.
wav=$1
log=$2
limit=${3:-0}
echo started >> "$log"
echo "loading model (log line, not an answer)"
echo "diagnostic on stderr" >&2
n=0
while read -r line; do
	out=$(printf '%s' "$line" | sed 's/.*"output_file": *"\([^"]*\)".*/\1/')
	cp "$wav" "$out"
	echo "$out"
	n=$((n + 1))
	if [ "$limit" -gt 0 ] && [ "$n" -ge "$limit" ]; then
		exit 0
	fi
done
