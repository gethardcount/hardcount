// Package recipe implements the pricing recipes feature: one-call
// instantiation of a pre-baked product+meter+pricing-rule+plan+dunning
// graph for AI-platform and SaaS pricing patterns. See
// docs/design/recipes.md.
package recipe

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"

	"github.com/sagarsuperuser/velox/internal/domain"
)

// recipeFS holds the bundled YAML definitions. Each *.yaml file under
// recipes/ is loaded once at package init and validated; a malformed file
// fails the binary at boot rather than at first instantiation.
//
//go:embed recipes/*.yaml
var recipeFS embed.FS

// Registry is the in-memory, immutable map of recipe key → parsed
// domain.Recipe. Built once at process start via Load(); read by Service
// without locks.
type Registry struct {
	byKey map[string]domain.Recipe
}

// Load reads every *.yaml under the embedded recipes/ directory, parses
// and validates each, and returns a Registry. A parse or validation
// failure on any file is fatal — the caller is expected to bubble the
// error up to main and abort.
func Load() (*Registry, error) {
	return load(recipeFS, "recipes")
}

// load is the testable form of Load — accepts an arbitrary fs.FS so unit
// tests can feed malformed fixtures.
func load(fsys fs.FS, dir string) (*Registry, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("recipe: read embedded dir: %w", err)
	}

	reg := &Registry{byKey: make(map[string]domain.Recipe, len(entries))}
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".yaml") {
			continue
		}
		data, err := fs.ReadFile(fsys, dir+"/"+ent.Name())
		if err != nil {
			return nil, fmt.Errorf("recipe: read %s: %w", ent.Name(), err)
		}
		recipe, err := parseRecipe(data)
		if err != nil {
			return nil, fmt.Errorf("recipe: parse %s: %w", ent.Name(), err)
		}
		if existing, dup := reg.byKey[recipe.Key]; dup {
			return nil, fmt.Errorf("recipe: duplicate key %q in %s and earlier file (version %s)", recipe.Key, ent.Name(), existing.Version)
		}
		reg.byKey[recipe.Key] = recipe
	}
	return reg, nil
}

// Get returns the recipe with the given key, or false if not found.
func (r *Registry) Get(key string) (domain.Recipe, bool) {
	rec, ok := r.byKey[key]
	return rec, ok
}

// List returns all loaded recipes sorted alphabetically by key. The
// caller treats the slice as read-only.
func (r *Registry) List() []domain.Recipe {
	out := make([]domain.Recipe, 0, len(r.byKey))
	for _, rec := range r.byKey {
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Keys returns just the keys, sorted. Useful for logging at boot.
func (r *Registry) Keys() []string {
	keys := make([]string, 0, len(r.byKey))
	for k := range r.byKey {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
