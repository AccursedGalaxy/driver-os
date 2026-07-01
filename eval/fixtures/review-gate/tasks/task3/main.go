// Command csvcut prints one column of a CSV stream. See SPEC.md.
package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: csvcut N")
		os.Exit(2)
	}
	col, err := strconv.Atoi(os.Args[1])
	if err != nil || col < 1 {
		fmt.Fprintln(os.Stderr, "csvcut: N must be a positive integer")
		os.Exit(2)
	}
	if err := run(os.Stdin, os.Stdout, col); err != nil {
		fmt.Fprintln(os.Stderr, "csvcut:", err)
		os.Exit(1)
	}
}

// run copies the col'th (1-based) field of every record in r to w.
func run(r io.Reader, w io.Writer, col int) error {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		fields := strings.Split(sc.Text(), ",")
		field := ""
		if col <= len(fields) {
			field = fields[col-1]
		}
		if _, err := fmt.Fprintln(w, field); err != nil {
			return err
		}
	}
	return sc.Err()
}
