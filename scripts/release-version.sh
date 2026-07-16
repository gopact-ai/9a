#!/bin/sh
set -eu

file=${1:-VERSION}
version=$(tr -d '\r\n' < "$file")
case "$version" in
	v[0-9]*.[0-9]*.[0-9]*) ;;
	*) exit 1 ;;
esac

major=${version#v}
major=${major%%.*}
minor=${version#v*.}
minor=${minor%%.*}
patch=${version##*.}
for part in "$major" "$minor" "$patch"; do
	case "$part" in
		0|[1-9]|[1-9][0-9]*) ;;
		*) exit 1 ;;
	esac
done
if [ "$version" != "v${major}.${minor}.${patch}" ]; then
	exit 1
fi
printf '%s\n' "$version"
