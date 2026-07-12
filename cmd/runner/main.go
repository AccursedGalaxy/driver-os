// Command runner is the reference runner: the headless agent loop with no
// optional capabilities armed — no ladder, no reviewer/planner roles, no
// modeled pricing table. It exists to demonstrate the core run contract
// (typed outcome, verify gates, evidence bundle) with the smallest possible
// dependency surface; the full driver binary wires the same headless package
// with every capability injected.
package main

import (
	"os"

	"github.com/AccursedGalaxy/driver-os/headless"
)

func main() {
	os.Exit(headless.Main(os.Args[1:]))
}
