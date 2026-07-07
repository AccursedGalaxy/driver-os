package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/AccursedGalaxy/driver-os/eval/suite/gobench"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("gobench-validate", flag.ContinueOnError)
	candidatesDir := fs.String("candidates", "gobench-mine-out/instances", "dir of candidate instance JSONs")
	oraclesRoot := fs.String("oracles", "gobench-mine-out/oracles", "ROOT oracles dir from the miner")
	outDir := fs.String("out", "gobench-validate-out", "output dir")
	cacheDir := fs.String("cache", filepath.Join(os.TempDir(), "gobench-validate-cache"), "bare-mirror cache dir")
	flakeRuns := fs.Int("flake-runs", 5, "number of flake check runs")
	idsFlag := fs.String("ids", "", "optional comma-separated instance_id filter")
	testTimeoutStr := fs.String("test-timeout", "10m", "per-run test timeout duration")

	if err := fs.Parse(args); err != nil {
		return 2
	}

	testTimeout, err := time.ParseDuration(*testTimeoutStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid -test-timeout: %v\n", err)
		return 1
	}

	if err := os.MkdirAll(filepath.Join(*outDir, "instances"), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create instances dir: %v\n", err)
		return 1
	}

	if fi, err := os.Stat(*candidatesDir); err != nil || !fi.IsDir() {
		fmt.Fprintf(os.Stderr, "candidates dir not readable: %s\n", *candidatesDir)
		return 1
	}

	matches, err := filepath.Glob(filepath.Join(*candidatesDir, "*.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "glob candidates: %v\n", err)
		return 1
	}

	var filter map[string]bool
	if *idsFlag != "" {
		filter = make(map[string]bool)
		for _, id := range strings.Split(*idsFlag, ",") {
			filter[strings.TrimSpace(id)] = true
		}
	}

	var rejections []*gobench.Rejection
	var acceptedCount int
	var totalCount int
	now := time.Now().Format(time.RFC3339)

	for _, path := range matches {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if filter != nil && !filter[id] {
			continue
		}
		totalCount++

		inst, err := gobench.LoadInstance(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "load %s: %v\n", path, err)
			rejections = append(rejections, &gobench.Rejection{
				InstanceID: id,
				Stage:      "infra",
				Reason:     "error",
				Detail:     fmt.Sprintf("load instance: %v", err),
			})
			continue
		}

		opts := gobench.ValidateOpts{
			OracleDir:   filepath.Join(*oraclesRoot, inst.InstanceID),
			CacheDir:    *cacheDir,
			FlakeRuns:   *flakeRuns,
			Now:         now,
			TestTimeout: testTimeout,
		}

		fmt.Printf("Validating %s...\n", inst.InstanceID)
		validated, rej, err := gobench.Validate(ctx, inst, opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error validating %s: %v\n", inst.InstanceID, err)
			rejections = append(rejections, &gobench.Rejection{
				InstanceID: inst.InstanceID,
				Repo:       inst.Repo,
				PR:         inst.PRNumber,
				Stage:      "infra",
				Reason:     "error",
				Detail:     err.Error(),
			})
			continue
		}

		if rej != nil {
			rejections = append(rejections, rej)
			continue
		}

		if err := writeInstance(*outDir, validated); err != nil {
			fmt.Fprintf(os.Stderr, "error writing %s: %v\n", inst.InstanceID, err)
			rejections = append(rejections, &gobench.Rejection{
				InstanceID: inst.InstanceID,
				Repo:       inst.Repo,
				PR:         inst.PRNumber,
				Stage:      "infra",
				Reason:     "error",
				Detail:     fmt.Sprintf("write instance: %v", err),
			})
			continue
		}
		acceptedCount++
	}

	if err := writeRejections(*outDir, rejections); err != nil {
		fmt.Fprintf(os.Stderr, "write rejections: %v\n", err)
		return 1
	}

	printSummary(totalCount, acceptedCount, rejections)

	return 0
}

func writeInstance(outDir string, inst gobench.Instance) error {
	path := filepath.Join(outDir, "instances", inst.InstanceID+".json")
	data, err := json.MarshalIndent(inst, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func writeRejections(outDir string, rejections []*gobench.Rejection) error {
	path := filepath.Join(outDir, "validation-rejections.jsonl")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, rej := range rejections {
		data, err := json.Marshal(rej)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func printSummary(total, accepted int, rejections []*gobench.Rejection) {
	reasons := map[string]int{
		"broken-base":         0,
		"base-testbuild-fail": 0,
		"overlay-collision":   0,
		"no-gate":             0,
		"f2p-did-not-run":     0,
		"gold-red":            0,
		"flaky":               0,
		"slow":                0,
		"error":               0,
	}
	for _, rej := range rejections {
		reasons[rej.Reason]++
	}

	fmt.Println()
	tw := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintf(tw, "Candidates\t%d\n", total)
	fmt.Fprintf(tw, "Accepted\t%d\n", accepted)
	fmt.Fprintf(tw, "Rejected\t%d\n", len(rejections))

	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(tw, "  - %s\t%d\n", k, reasons[k])
	}
	tw.Flush()
}
