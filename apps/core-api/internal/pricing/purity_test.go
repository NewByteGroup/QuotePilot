package pricing

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Task 8 (spec §10): the purity guard — the package must stay pure so
// the same input always produces the same output. Two checks: a strict
// import whitelist on non-test files, and a token scan banning clock
// reads, floats and interface{} in package code.

var allowedImports = map[string]bool{
	"crypto/sha256":                 true,
	"encoding/hex":                  true,
	"encoding/json":                 true,
	"errors":                        true,
	"fmt":                           true,
	"sort":                          true,
	"strconv":                       true,
	"strings":                       true,
	"time":                          true,
	"github.com/google/uuid":        true,
	"github.com/shopspring/decimal": true,
}

var bannedTokens = []string{
	"time.Now(",
	"time.Since(",
	"float64",
	"NewFromFloat",
	"interface{}",
	"math/rand",
}

func TestPurity_ImportsWhitelisted(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !allowedImports[path] {
				t.Errorf("%s: import %q not on the pure-package whitelist", e, path)
			}
		}
	}
}

func TestPurity_NoBannedTokens(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e, "_test.go") {
			continue
		}
		src, err := os.ReadFile(e)
		if err != nil {
			t.Fatalf("read %s: %v", e, err)
		}
		for _, tok := range bannedTokens {
			if strings.Contains(string(src), tok) {
				t.Errorf("%s: banned token %q found — the engine must stay pure and decimal-only", e, tok)
			}
		}
	}
}
