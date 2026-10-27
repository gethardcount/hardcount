package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A request body Velox defines is read by internal/api/respond, which
// refuses a key the request type does not declare (422, param = the key) and
// a body that is not one JSON value (400). encoding/json on its own drops an
// unknown key, so a misspelled field answered 200 and changed nothing.
//
// This pins every function (outside tests) that reads an *http.Request's
// Body. A new reader fails until it decodes through respond, or is added
// here with the reason its body is not ours to define.
var requestBodyReaders = map[string]string{
	"internal/api/respond/decode.go decodeBody": "the one strict decoder",

	// Reads the bytes to fingerprint the request and puts them back; the
	// handler decodes them.
	"internal/api/middleware/idempotency.go Idempotency": "replay fingerprint",

	// Senders Velox does not control. They add fields when they like, so
	// these read only what they need and ignore the rest.
	"internal/payment/handler.go Handler.handleStripeWebhook":          "Stripe webhook",
	"internal/email/postmark_handler.go PostmarkHandler.HandleWebhook": "Postmark webhook",
	"internal/integrations/litellm/handler.go decodeBody":              "LiteLLM spend callback",
}

// requestBodyReads returns every non-test function under internal/ and
// cmd/ that reads the Body of one of its *http.Request parameters: passes
// it to a call other than http.MaxBytesReader, or calls a method on it other
// than Close.
func requestBodyReads(t *testing.T, root string) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				if readsRequestBody(fd) {
					found[rel+" "+funcKey(fd)] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", top, err)
		}
	}
	return found
}

// readsRequestBody reports whether fd, or a function literal inside it,
// reads the Body of a *http.Request parameter.
func readsRequestBody(fd *ast.FuncDecl) bool {
	reqs := map[string]bool{}
	addParams := func(ft *ast.FuncType) {
		if ft == nil || ft.Params == nil {
			return
		}
		for _, field := range ft.Params.List {
			if isHTTPRequestPtr(field.Type) {
				for _, n := range field.Names {
					reqs[n.Name] = true
				}
			}
		}
	}
	addParams(fd.Type)
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if fl, ok := n.(*ast.FuncLit); ok {
			addParams(fl.Type)
		}
		return true
	})
	if len(reqs) == 0 {
		return false
	}
	isBody := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Body" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && reqs[id.Name]
	}
	hit := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return !hit
		}
		if fun, ok := call.Fun.(*ast.SelectorExpr); ok {
			if isBody(fun.X) && fun.Sel.Name != "Close" {
				hit = true
			}
			if pkg, ok := fun.X.(*ast.Ident); ok && pkg.Name == "http" && fun.Sel.Name == "MaxBytesReader" {
				return !hit
			}
		}
		for _, arg := range call.Args {
			if isBody(arg) {
				hit = true
			}
		}
		return !hit
	})
	return hit
}

func isHTTPRequestPtr(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Request" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "http"
}

func TestNoRequestBodyDecodeOutsideRespond(t *testing.T) {
	root := repoRoot(t)
	found := requestBodyReads(t, root)

	var missing, stale []string
	for k := range found {
		if _, ok := requestBodyReaders[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k, why := range requestBodyReaders {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s: no reason given", k)
		}
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, k := range missing {
		t.Errorf("%s reads a request body itself. Decode it with respond.DecodeStrict (or respond.DecodeBody), "+
			"which refuses unknown keys. Only a body whose sender Velox does not control may be read here; "+
			"add it to requestBodyReaders with the reason.", k)
	}
	for _, k := range stale {
		t.Errorf("%s is listed in requestBodyReaders but no longer reads a request body; remove it.", k)
	}
}

// TestReadsRequestBody_Patterns pins what counts as reading a body. A wrapped
// or aliased body is still read; only closing it, or capping it in place
// with http.MaxBytesReader before the strict decoder reads it, is not.
func TestReadsRequestBody_Patterns(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"direct decode", `json.NewDecoder(r.Body).Decode(&v)`, true},
		{"io.ReadAll", `io.ReadAll(r.Body)`, true},
		{"method on the body", `r.Body.Read(buf)`, true},
		{"wrapped in MaxBytesReader", `json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&v)`, true},
		{"aliased first", `body := r.Body; json.NewDecoder(body).Decode(&v)`, true},
		{"read in a closure", `func() { io.ReadAll(r.Body) }()`, true},
		{"capped in place, then the strict decoder", `r.Body = http.MaxBytesReader(w, r.Body, 1<<20); respond.DecodeStrict(w, r, &v)`, false},
		{"closed", `defer r.Body.Close()`, false},
		{"nil check", `if r.Body == nil { return }`, false},
		{"strict decoder", `respond.DecodeStrict(w, r, &v)`, false},
	}
	for _, tc := range cases {
		src := "package p\nfunc h(w http.ResponseWriter, r *http.Request) {\n" + tc.src + "\n}\n"
		f, err := parser.ParseFile(token.NewFileSet(), "p.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		fd := f.Decls[0].(*ast.FuncDecl)
		if got := readsRequestBody(fd); got != tc.want {
			t.Errorf("%s: readsRequestBody = %v, want %v", tc.name, got, tc.want)
		}
	}
}
