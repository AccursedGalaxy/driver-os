package agent

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"strings"
)

func fileOutline(path string, lines []string) string {
	var symbols []string
	isGo := strings.HasSuffix(path, ".go")
	if isGo {
		symbols = goOutline(path, lines)
	}
	if len(symbols) == 0 && !isGo {
		symbols = genericOutline(lines)
	}
	// If Go parsing returned a nil file (e.g. catastrophic parse error),
	// we still want the generic fallback even for .go files.
	if len(symbols) == 0 && isGo {
		fset := token.NewFileSet()
		src := strings.Join(lines, "\n")
		f, _ := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if f == nil {
			symbols = genericOutline(lines)
		}
	}
	if len(symbols) == 0 {
		return ""
	}

	total := len(symbols)
	if len(symbols) > 50 {
		symbols = symbols[:50]
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- file outline (%d symbols) — read a range like `%s:120-160` to see one ---", total, path)
	for _, s := range symbols {
		b.WriteByte('\n')
		b.WriteString(s)
	}
	if total > 50 {
		fmt.Fprintf(&b, "\n  … (+%d more symbols)", total-50)
	}
	return b.String()
}

func goOutline(path string, lines []string) []string {
	fset := token.NewFileSet()
	src := strings.Join(lines, "\n")
	f, _ := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
	if f == nil {
		return nil
	}

	var symbols []string
	for _, decl := range f.Decls {
		line := fset.Position(decl.Pos()).Line
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv := renderType(d.Recv.List[0].Type)
				symbols = append(symbols, fmt.Sprintf("%4d func (%s) %s", line, recv, name))
			} else {
				symbols = append(symbols, fmt.Sprintf("%4d func %s", line, name))
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					symbols = append(symbols, fmt.Sprintf("%4d type %s", line, s.Name.Name))
				case *ast.ValueSpec:
					for _, name := range s.Names {
						kind := "var"
						if d.Tok == token.CONST {
							kind = "const"
						}
						symbols = append(symbols, fmt.Sprintf("%4d %s %s", line, kind, name.Name))
					}
				}
			}
		}
	}
	return symbols
}

func renderType(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + renderType(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr: // Generic type: T[P]
		return renderType(t.X) + "[...]"
	case *ast.SelectorExpr:
		return renderType(t.X) + "." + t.Sel.Name
	default:
		return fmt.Sprintf("%T", expr)
	}
}

var genericDefCue = regexp.MustCompile(`^(package |import |func |type |class |def |function |interface |struct |impl |fn |module |const |var |public |private |export )`)

func genericOutline(lines []string) []string {
	var symbols []string
	for i, line := range lines {
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if genericDefCue.MatchString(line) {
			trimmed := strings.TrimSpace(line)
			if len(trimmed) > 80 {
				trimmed = trimmed[:77] + "..."
			}
			symbols = append(symbols, fmt.Sprintf("%4d %s", i+1, trimmed))
		}
	}
	return symbols
}
