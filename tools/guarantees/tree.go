package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// storetestDir holds the conformance suite of index.Store, whose cases the document names
	// as "the store conformance case `Sweep`".
	storetestDir = "internal/index/storetest"
	// e2eDir holds the end-to-end scripts, named by their file name.
	e2eDir = "test/e2e"
)

// goTest is one Go test function of the tree.
type goTest struct {
	pkg     string // the package's name, without _test
	dir     string // the package's directory, slash-separated from the root
	skipped bool   // its body calls t.Skip, t.Skipf or t.SkipNow outside any condition
}

// tree is what the document may name: the Go tests of every _test.go file under every build
// tag, the store conformance cases, and the end-to-end scripts with their text.
type tree struct {
	tests    map[string][]goTest // by function name
	packages map[string]bool     // the names and the directories of the packages with tests
	cases    map[string]bool
	scripts  map[string]string // by file name, test/e2e/<name>
}

// load reads the tree of fsys, a repository's root. Directories whose name starts with a dot,
// testdata and node_modules are skipped.
func load(fsys fs.FS) (*tree, error) {
	t := &tree{tests: map[string][]goTest{}, packages: map[string]bool{}, cases: map[string]bool{},
		scripts: map[string]string{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if p != "." && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return fs.SkipDir
			}
			return nil
		}
		switch {
		case strings.HasSuffix(name, "_test.go"):
			return t.addTests(fsys, p)
		case path.Dir(p) == storetestDir && strings.HasSuffix(name, ".go"):
			return t.addCases(fsys, p)
		case path.Dir(p) == e2eDir && strings.HasSuffix(name, ".sh"):
			b, err := fs.ReadFile(fsys, p)
			t.scripts[name] = string(b)
			return err
		}
		return nil
	})
	return t, err
}

// parseGo parses the Go file p of fsys.
func parseGo(fsys fs.FS, p string) (*ast.File, error) {
	src, err := fs.ReadFile(fsys, p)
	if err != nil {
		return nil, err
	}
	f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("guarantees: %w", err)
	}
	return f, nil
}

// addTests records the test functions of the _test.go file p.
func (t *tree) addTests(fsys fs.FS, p string) error {
	f, err := parseGo(fsys, p)
	if err != nil {
		return err
	}
	pkg := strings.TrimSuffix(f.Name.Name, "_test")
	dir := path.Dir(p)
	t.packages[pkg], t.packages[dir] = true, true
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isTestName(fn.Name.Name) {
			continue
		}
		t.tests[fn.Name.Name] = append(t.tests[fn.Name.Name], goTest{pkg: pkg, dir: dir, skipped: skipsAlways(fn)})
	}
	return nil
}

// addCases records the conformance cases of the storetest file p: the elements {"Name", testName}
// of its table.
func (t *tree) addCases(fsys fs.FS, p string) error {
	f, err := parseGo(fsys, p)
	if err != nil {
		return err
	}
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || len(lit.Elts) != 2 {
			return true
		}
		name, ok1 := lit.Elts[0].(*ast.BasicLit)
		fn, ok2 := lit.Elts[1].(*ast.Ident)
		if ok1 && ok2 && name.Kind == token.STRING && strings.HasPrefix(fn.Name, "test") {
			if s, err := strconv.Unquote(name.Value); err == nil {
				t.cases[s] = true
			}
		}
		return true
	})
	return nil
}

// isTestName reports whether name is a name go test runs: Test, Fuzz, Benchmark or Example,
// then nothing or a character that is not a lower-case letter.
func isTestName(name string) bool {
	for _, prefix := range []string{"Test", "Fuzz", "Benchmark", "Example"} {
		rest, ok := strings.CutPrefix(name, prefix)
		if !ok {
			continue
		}
		r, _ := utf8.DecodeRuneInString(rest)
		return rest == "" || !unicode.IsLower(r)
	}
	return false
}

// skipsAlways reports whether a statement of fn's body, not one inside a condition, is
// <param>.Skip, Skipf or SkipNow of fn's first parameter.
func skipsAlways(fn *ast.FuncDecl) bool {
	params := fn.Type.Params.List
	if fn.Body == nil || len(params) == 0 || len(params[0].Names) == 0 {
		return false
	}
	param := params[0].Names[0].Name
	for _, st := range fn.Body.List {
		es, ok := st.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := es.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == param {
			switch sel.Sel.Name {
			case "Skip", "Skipf", "SkipNow":
				return true
			}
		}
	}
	return false
}
