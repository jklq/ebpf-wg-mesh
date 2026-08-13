#!/usr/bin/env zsh

emulate -L zsh
setopt errexit nounset pipefail

cd "${0:A:h}/.."

typeset -A package_patterns
typeset -a integration_files test_names

integration_files=("${(@f)$(rg -l '^//go:build integration$' --glob '*_test.go')}")

if (( ${#integration_files} == 0 )); then
	print "no integration-tagged Go test files found"
	exit 0
fi

for file in $integration_files; do
	# Helper files (harnesses, fixtures) are integration-tagged but have no Test*.
	# rg exits 1 on no match; keep going instead of aborting the suite.
	test_names=("${(@f)$(rg -o 'func (Test[^ (]+)\(' -r '$1' -- "$file" || true)}")
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

for package_dir in ${(ok)package_patterns}; do
	print "==> go test -count=1 -v -tags=integration ./$package_dir"
	go test -count=1 -v -tags=integration "./${package_dir}" -run "^(${package_patterns[$package_dir]})$"
done
