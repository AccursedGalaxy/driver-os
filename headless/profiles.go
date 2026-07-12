package headless

import (
	"flag"
	"sort"
	"strings"

	"github.com/AccursedGalaxy/driver-os/profile"
)

// mustCoding returns the current headless default profile.
func mustCoding() profile.Exec {
	e, err := profile.ExecByName("coding-v2")
	if err != nil {
		panic(err)
	}
	return e
}

// profileFromArgs resolves -profile before FlagSet registration, because flag
// defaults are fixed at registration time. The last occurrence wins, matching
// the flag package's normal repeated-flag behavior.
func profileFromArgs(args []string) (profile.Exec, error) {
	name := "coding-v2"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-profile" || arg == "--profile":
			if i+1 < len(args) {
				i++
				name = args[i]
			}
		case strings.HasPrefix(arg, "-profile="):
			name = strings.TrimPrefix(arg, "-profile=")
		case strings.HasPrefix(arg, "--profile="):
			name = strings.TrimPrefix(arg, "--profile=")
		}
	}
	return profile.ExecByName(name)
}

// cliOverrides returns the explicitly-set flag names in deterministic order.
func cliOverrides(fs *flag.FlagSet) []string {
	var names []string
	fs.Visit(func(f *flag.Flag) { names = append(names, f.Name) })
	sort.Strings(names)
	return names
}
