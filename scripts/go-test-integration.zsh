#!/usr/bin/env zsh

emulate -L zsh
setopt errexit nounset pipefail

cd "${0:A:h}/.."

typeset -A package_patterns
typeset -A package_test_indexes
typeset -a integration_files test_names

shard_count_value=${INTEGRATION_SHARD_COUNT:-1}
shard_index_value=${INTEGRATION_SHARD_INDEX:-0}
if [[ $shard_count_value != <-> || $shard_count_value == 0 ]]; then
	print -u2 "INTEGRATION_SHARD_COUNT must be a positive integer"
	exit 1
fi
if [[ $shard_index_value != <-> || $shard_index_value -ge $shard_count_value ]]; then
	print -u2 "INTEGRATION_SHARD_INDEX must be an integer in [0, INTEGRATION_SHARD_COUNT)"
	exit 1
fi
typeset -i shard_count=$shard_count_value
typeset -i shard_index=$shard_index_value

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
		test_index=${package_test_indexes[${file:h}]:-0}
		package_test_indexes[${file:h}]=$(( test_index + 1 ))
		if (( test_index % shard_count != shard_index )); then
			continue
		fi
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
	print "==> go test integration shard $(( shard_index + 1 ))/$shard_count: ./$package_dir"
	go test -count=1 -v -tags=integration "./${package_dir}" -run "^(${package_patterns[$package_dir]})$"
done
