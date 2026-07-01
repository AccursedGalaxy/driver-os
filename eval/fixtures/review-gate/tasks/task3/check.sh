#!/bin/sh
# Behavioral conformance check: build the tool and compare its output on the
# fixture input against the expected output (see SPEC.md).
set -e
cd "$(dirname "$0")"
go build -o /tmp/csvcut-check .
/tmp/csvcut-check 2 < testdata/input.csv > /tmp/csvcut-got.txt
diff -u testdata/expected.txt /tmp/csvcut-got.txt
echo "conformance: OK"
