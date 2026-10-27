package arch

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Every error the API answers goes through internal/api/respond, so it has
// one shape: {"error": {"type", "code", "message", "param", "request_id"}},
// the Velox-Request-Id header and Content-Type application/json.
//
// This finds, in non-test code under internal/ and cmd/, every place that
// writes a response status or body by hand:
//
//   - http.Error, http.NotFound, http.NotFoundHandler;
//   - WriteHeader with anything but a 1xx-3xx status constant;
//   - respond.JSON with anything but a 1xx-3xx status constant (an error
//     body written as a plain success value);
//   - json.NewEncoder, fmt.Fprint* or Write on an http.ResponseWriter;
//   - chi middleware that answers errors itself (Recoverer, Timeout, ...).
//
// A new one fails until it goes through respond, or is listed here with the
// reason it is not an error answer. Keys are "file function".
var responseWriteExceptions = map[string]string{
	// The replay of a stored first response, byte for byte. The first
	// response was written by respond.
	"internal/api/middleware/idempotency.go replayExistingKey": "replays the stored first response",

	// Wrappers that record the status and pass the call through.
	"internal/api/middleware/idempotency.go responseRecorder.WriteHeader": "pass-through recorder",
	"internal/api/middleware/idempotency.go responseRecorder.Write":       "pass-through recorder",
	"internal/user/handler.go statusRecorder.WriteHeader":                 "pass-through recorder",

	// Success answers whose status is a variable (200 or 201).
	"internal/pricing/handler.go Handler.respondMeter":    "success: 200 or 201",
	"internal/pricing/handler.go Handler.respondPlan":     "success: 200 or 201",
	"internal/subscription/handler.go Handler.respondSub": "success: 200 or 201",
	"internal/billing/handler.go Handler.triggerCycle":    "success: 200, or 206 with the per-subscription errors",

	// The readiness probe: a load balancer reads the status, and the body
	// names the failing check.
	"internal/api/router.go handleDeepHealth": "readiness probe: 503 with {status, checks}",

	// Success bodies that are not JSON.
	"internal/creditnote/handler.go Handler.downloadPDF":    "PDF body",
	"internal/invoice/handler.go Handler.downloadPDF":       "PDF body",
	"internal/hostedinvoice/handler.go Handler.downloadPDF": "PDF body",
	"internal/webhook/sse.go Handler.streamEvents":          "server-sent event stream",
	"internal/webhook/sse.go writeFrame":                    "server-sent event frames",
}

// chiErrorMiddleware is chi middleware that writes an error answer of its own
// (plain text or an empty body).
var chiErrorMiddleware = map[string]bool{
	"Recoverer": true, "Timeout": true, "Throttle": true, "ThrottleBacklog": true,
	"ThrottleWithOpts": true, "AllowContentType": true, "AllowContentEncoding": true,
	"ContentCharset": true, "BasicAuth": true, "Heartbeat": true,
}

// successStatus are the status constants a hand-written status may use.
var successStatus = map[string]bool{
	"StatusContinue": true, "StatusSwitchingProtocols": true,
	"StatusOK": true, "StatusCreated": true, "StatusAccepted": true, "StatusNoContent": true,
	"StatusPartialContent": true, "StatusMultiStatus": true,
	"StatusMovedPermanently": true, "StatusFound": true, "StatusSeeOther": true,
	"StatusNotModified": true, "StatusTemporaryRedirect": true, "StatusPermanentRedirect": true,
}

func TestResponseWrites_ErrorsGoThroughRespond(t *testing.T) {
	root := repoRoot(t)
	found := map[string][]string{}
	always := map[string]bool{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, p)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == "internal/api/respond" || rel == "internal/api/generated" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			chiMW := importName(f, "github.com/go-chi/chi/v5/middleware", "middleware")
			for _, decl := range f.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				for _, w := range responseWrites(fd, chiMW) {
					key := rel + " " + funcKey(fd)
					found[key] = append(found[key], fset.Position(w.pos).String()[len(root)+1:]+": "+w.what)
					always[key] = always[key] || w.always
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s/: %v", top, err)
		}
	}

	var keys []string
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := responseWriteExceptions[k]; ok && !always[k] {
			continue
		}
		t.Errorf("%s writes a response by hand:\n  %s\nAnswer errors with internal/api/respond (respond.Error and its helpers), "+
			"or, if this is not an error answer, add it to responseWriteExceptions with the reason.",
			k, strings.Join(found[k], "\n  "))
	}
	for k := range responseWriteExceptions {
		if _, ok := found[k]; !ok {
			t.Errorf("responseWriteExceptions names %s, which no longer writes a response by hand: remove it", k)
		}
	}
	if len(found) == 0 {
		t.Fatal("found no hand-written response at all: the scan is broken")
	}
}

type responseWrite struct {
	pos  token.Pos
	what string
	// always: no exception covers it (http.Error and chi's error
	// middleware only ever write errors).
	always bool
}

// responseWrites lists the hand-written response writes in fd, including in
// function literals inside it. chiMW is the local name of chi's middleware
// package in the file, or "" when the file does not import it.
func responseWrites(fd *ast.FuncDecl, chiMW string) []responseWrite {
	writers := map[string]bool{}
	addParams := func(ft *ast.FuncType) {
		if ft == nil || ft.Params == nil {
			return
		}
		for _, field := range ft.Params.List {
			if sel, ok := field.Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "ResponseWriter" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "http" {
					for _, n := range field.Names {
						writers[n.Name] = true
					}
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
	isWriter := func(e ast.Expr) bool {
		id, ok := e.(*ast.Ident)
		return ok && writers[id.Name]
	}

	var out []responseWrite
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if x, ok := n.X.(*ast.Ident); ok && chiMW != "" && x.Name == chiMW && chiErrorMiddleware[n.Sel.Name] {
				out = append(out, responseWrite{n.Pos(), "chi middleware." + n.Sel.Name + " writes its own error answer", true})
			}
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, _ := sel.X.(*ast.Ident)
			switch {
			case pkg != nil && pkg.Name == "http" && (sel.Sel.Name == "Error" || sel.Sel.Name == "NotFound" || sel.Sel.Name == "NotFoundHandler"):
				out = append(out, responseWrite{n.Pos(), "http." + sel.Sel.Name, true})
			case sel.Sel.Name == "WriteHeader" && len(n.Args) == 1 && !isSuccessStatus(n.Args[0]):
				out = append(out, responseWrite{n.Pos(), "WriteHeader with a status that is not a 1xx-3xx constant", false})
			case pkg != nil && pkg.Name == "respond" && sel.Sel.Name == "JSON" && len(n.Args) >= 3 && !isSuccessStatus(n.Args[2]):
				out = append(out, responseWrite{n.Pos(), "respond.JSON with a status that is not a 1xx-3xx constant (use respond.Error)", false})
			case pkg != nil && pkg.Name == "json" && sel.Sel.Name == "NewEncoder" && len(n.Args) == 1 && isWriter(n.Args[0]):
				out = append(out, responseWrite{n.Pos(), "json.NewEncoder on the ResponseWriter", false})
			case pkg != nil && pkg.Name == "fmt" && strings.HasPrefix(sel.Sel.Name, "Fprint") && len(n.Args) >= 1 && isWriter(n.Args[0]):
				out = append(out, responseWrite{n.Pos(), "fmt." + sel.Sel.Name + " on the ResponseWriter", false})
			case pkg != nil && pkg.Name == "io" && sel.Sel.Name == "WriteString" && len(n.Args) >= 1 && isWriter(n.Args[0]):
				out = append(out, responseWrite{n.Pos(), "io.WriteString on the ResponseWriter", false})
			case sel.Sel.Name == "Write" && (isWriter(sel.X) || isEmbeddedWriter(sel.X)):
				out = append(out, responseWrite{n.Pos(), "Write on the ResponseWriter", false})
			}
		}
		return true
	})
	return out
}

// isEmbeddedWriter matches x.ResponseWriter, the embedded writer of a
// recorder type.
func isEmbeddedWriter(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "ResponseWriter"
}

// isSuccessStatus reports whether e is an http.Status constant below 400, or
// an integer literal below 400.
func isSuccessStatus(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && x.Name == "http" && successStatus[e.Sel.Name]
	case *ast.BasicLit:
		n, err := strconv.Atoi(e.Value)
		return e.Kind == token.INT && err == nil && n < 400
	}
	return false
}

// importName is the name a file uses for the package at path: its alias, or
// def when it imports it without one. "" when the file does not import it.
func importName(f *ast.File, path, def string) string {
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != path {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return def
	}
	return ""
}
