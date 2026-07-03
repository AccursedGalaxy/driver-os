// Module boundary on purpose: this oracle package is deliberately
// non-compiling standalone (its *_test.go references symbols that only exist
// once copied into a materialized gaps/ workspace by
// eval/scripts/ps4-gap-baseline.sh, which copies *.go only — never this file).
// The nested module keeps the parent module's `go test ./...` / `go vet ./...`
// green.
module retry

go 1.24
