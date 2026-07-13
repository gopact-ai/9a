#!/bin/sh
set -eu

version=${1:-v0.0.0}
case "$version" in v*.*.*) ;; *) exit 1 ;; esac
major=${version#v}
major=${major%%.*}
minor=${version#v*.}
minor=${minor%%.*}
patch=${version##*.}
for part in "$major" "$minor" "$patch"; do
	case "$part" in ''|*[!0-9]*) exit 1 ;; esac
done
printf '%s.%s\n' "${version%.*}" "$((patch + 1))"
