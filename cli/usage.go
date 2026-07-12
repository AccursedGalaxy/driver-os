package cli

import (
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// UsageGroup is one titled section of a front-end's -help output.
type UsageGroup struct {
	Title string
	Flags []string // flag names in display order
}

// GroupedUsage returns a flag.Usage function that renders fs's flags in titled
// groups instead of one alphabetical wall. Flags not named in any group land in
// a trailing "Other" section, so a newly added flag is never invisible in -help
// (and a test can assert the section stays empty). The per-flag rendering
// follows flag.PrintDefaults conventions: name, argument type derived from
// backquotes in the usage string, the usage text, and a non-zero default.
func GroupedUsage(fs *flag.FlagSet, w io.Writer, header string, groups []UsageGroup) func() {
	return func() {
		fmt.Fprintln(w, header)
		claimed := make(map[string]bool)
		for _, g := range groups {
			for _, name := range g.Flags {
				claimed[name] = true
			}
		}
		byName := make(map[string]*flag.Flag)
		var leftover []string
		fs.VisitAll(func(f *flag.Flag) {
			byName[f.Name] = f
			if !claimed[f.Name] {
				leftover = append(leftover, f.Name)
			}
		})
		printGroup := func(title string, names []string) {
			printed := false
			for _, name := range names {
				f, ok := byName[name]
				if !ok {
					continue // grouped name without a defined flag: skip silently; the test catches it
				}
				if !printed {
					fmt.Fprintf(w, "\n%s:\n", title)
					printed = true
				}
				printFlag(w, f)
			}
		}
		for _, g := range groups {
			printGroup(g.Title, g.Flags)
		}
		printGroup("Other", leftover)
	}
}

// printFlag renders one flag in the stdlib PrintDefaults shape.
func printFlag(w io.Writer, f *flag.Flag) {
	name, usage := flag.UnquoteUsage(f)
	line := "  -" + f.Name
	if name != "" {
		line += " " + name
	}
	fmt.Fprintln(w, line)
	if def := f.DefValue; def != "" && !isZeroDefault(def) {
		usage += fmt.Sprintf(" (default %s)", maybeQuote(def))
	}
	fmt.Fprintf(w, "    \t%s\n", strings.ReplaceAll(usage, "\n", "\n    \t"))
}

// isZeroDefault reports whether a DefValue is the type's zero value, which
// PrintDefaults conventionally omits.
func isZeroDefault(def string) bool {
	switch def {
	case "false", "0", "0s", "":
		return true
	}
	return false
}

// maybeQuote quotes string-ish defaults the way PrintDefaults does (anything
// that isn't a bare number/bool/duration reads better quoted).
func maybeQuote(def string) string {
	if def == "true" || def == "false" {
		return def
	}
	if _, err := strconv.ParseFloat(def, 64); err == nil {
		return def
	}
	if _, err := time.ParseDuration(def); err == nil {
		return def
	}
	return strconv.Quote(def)
}

// UncategorizedFlags returns the names fs defines that groups do not claim, and
// the names groups claim that fs does not define. Front-end tests assert both
// are empty so -help grouping can never silently drift from the flag set.
func UncategorizedFlags(fs *flag.FlagSet, groups []UsageGroup) (missing, stale []string) {
	claimed := make(map[string]bool)
	for _, g := range groups {
		for _, name := range g.Flags {
			claimed[name] = true
		}
	}
	defined := make(map[string]bool)
	fs.VisitAll(func(f *flag.Flag) {
		defined[f.Name] = true
		if !claimed[f.Name] {
			missing = append(missing, f.Name)
		}
	})
	for name := range claimed {
		if !defined[name] {
			stale = append(stale, name)
		}
	}
	return missing, stale
}
