#!/usr/bin/env zsh

emulate -L zsh
setopt errexit nounset pipefail

cd "${0:A:h}/.."

typeset -A package_patterns
typeset -a integration_files test_names

# Discover integration suites from tracked files only. `git grep` is
# deterministic in a checkout and ignores local ignore rules that can silently
# hide files from a bare recursive search.
integration_files=("${(@f)$(git grep -l -e '^//go:build integration$' -- '*_test.go' || true)}")
integration_files=(${integration_files:#})

if (( ${#integration_files} == 0 )); then
	print -u2 "no integration-tagged Go test files found"
	exit 1
fi

for file in $integration_files; do
	# Helper files (harnesses, fixtures) are integration-tagged but have no Test*.
	# grep exits 1 on no match; keep going instead of aborting the suite.
	test_names=("${(@f)$(grep -oE 'func Test[^ (]+\(' -- "$file" | sed -E 's/^func (Test[^ (]+)\($/\1/' || true)}")
	test_names=(${test_names:#})
	if (( ${#test_names} == 0 )); then
		continue
	fi

	for test_name in $test_names; do
		if [[ -n ${package_patterns[${file:h}]-} ]]; then
			package_patterns[${file:h}]+="|${test_name}"
		else
			package_patterns[${file:h}]="${test_name}"
		fi
	done
done

if (( ${#package_patterns} == 0 )); then
	print -u2 "no integration test functions found"
	exit 1
fi

for package_dir in ${(ok)package_patterns}; do
	print "==> go test -count=1 -v -tags=integration ./$package_dir"
	go test -count=1 -v -tags=integration "./${package_dir}" -run "^(${package_patterns[$package_dir]})$"
done
