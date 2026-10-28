package recipe

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"text/template"

	"github.com/gethardcount/hardcount/internal/domain"
)

// renderRecipe applies overrides to every template-bearing string in r and
// returns a copy with all `{{ .X }}` substitutions resolved. v1 supports
// pure substitution only — no conditionals, no loops, no functions. The
// override map is built by overlaying caller-provided overrides on top of
// the recipe's declared defaults.
func renderRecipe(r domain.Recipe, overrides map[string]any) (domain.Recipe, error) {
	merged, err := mergeOverrides(r, overrides)
	if err != nil {
		return domain.Recipe{}, err
	}

	out := r
	out.Overridable = nil // overrides are an input, not part of rendered output

	apply := func(s string) (string, error) {
		if s == "" || !strings.Contains(s, "{{") {
			return s, nil
		}
		t, err := template.New("recipe").Option("missingkey=error").Parse(s)
		if err != nil {
			return "", fmt.Errorf("template parse: %w", err)
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, merged); err != nil {
			return "", fmt.Errorf("template execute: %w", err)
		}
		return buf.String(), nil
	}

	out.Name, err = apply(out.Name)
	if err != nil {
		return domain.Recipe{}, err
	}
	out.Summary, err = apply(out.Summary)
	if err != nil {
		return domain.Recipe{}, err
	}
	out.Description, err = apply(out.Description)
	if err != nil {
		return domain.Recipe{}, err
	}

	out.Plans = append([]domain.RecipePlan(nil), r.Plans...)
	for i, p := range out.Plans {
		if out.Plans[i].Code, err = apply(p.Code); err != nil {
			return domain.Recipe{}, err
		}
		if out.Plans[i].Name, err = apply(p.Name); err != nil {
			return domain.Recipe{}, err
		}
		if out.Plans[i].Currency, err = apply(p.Currency); err != nil {
			return domain.Recipe{}, err
		}
	}

	return out, nil
}

// mergeOverrides validates each caller-supplied override against the
// recipe's `overridable` declarations and returns a complete map (defaults
// for keys the caller omitted, validated values for keys it supplied).
// Unknown override keys are an error: silent ignores would mask
// dashboard-side typos.
func mergeOverrides(r domain.Recipe, overrides map[string]any) (map[string]any, error) {
	allowed := make(map[string]domain.RecipeOverride, len(r.Overridable))
	for _, ov := range r.Overridable {
		allowed[ov.Key] = ov
	}

	for k := range overrides {
		if _, ok := allowed[k]; !ok {
			return nil, fmt.Errorf("unknown override key %q for recipe %q", k, r.Key)
		}
	}

	out := make(map[string]any, len(allowed))
	for _, ov := range r.Overridable {
		v, ok := overrides[ov.Key]
		if !ok {
			out[ov.Key] = ov.Default
			continue
		}
		if err := validateOverrideValue(ov, v); err != nil {
			return nil, err
		}
		out[ov.Key] = v
	}
	return out, nil
}

func validateOverrideValue(ov domain.RecipeOverride, v any) error {
	switch ov.Type {
	case "string":
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("override %q must be string, got %T", ov.Key, v)
		}
		if ov.MaxLength > 0 && len(s) > ov.MaxLength {
			return fmt.Errorf("override %q exceeds max length %d", ov.Key, ov.MaxLength)
		}
		if len(ov.Enum) > 0 {
			found := false
			for _, e := range ov.Enum {
				if s == e {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("override %q value %q not in allowed enum %v", ov.Key, s, ov.Enum)
			}
		}
		if ov.Pattern != "" {
			re, err := regexp.Compile(ov.Pattern)
			if err != nil {
				return fmt.Errorf("override %q pattern compile: %w", ov.Key, err)
			}
			if !re.MatchString(s) {
				return fmt.Errorf("override %q value %q does not match pattern %s", ov.Key, s, ov.Pattern)
			}
		}
	case "int":
		switch v.(type) {
		case int, int32, int64, float64:
			// JSON unmarshal yields float64 for numbers; accept and let the consumer cast.
		default:
			return fmt.Errorf("override %q must be int, got %T", ov.Key, v)
		}
	}
	return nil
}

// templateRefPattern matches `{{ .X }}` references — used to validate at
// recipe load time that every template variable resolves to a declared
// override key. Loose matcher: tolerates whitespace and dotted form.
var templateRefPattern = regexp.MustCompile(`\{\{\s*\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// validateTemplateReferences scans every template-bearing field in r and
// asserts that each `{{ .X }}` reference matches a declared override key.
// Catches typos at boot rather than at first instantiation.
func validateTemplateReferences(r domain.Recipe, allowed map[string]struct{}) error {
	check := func(s, where string) error {
		for _, m := range templateRefPattern.FindAllStringSubmatch(s, -1) {
			if _, ok := allowed[m[1]]; !ok {
				return fmt.Errorf("recipe %q: %s references undeclared override %q", r.Key, where, m[1])
			}
		}
		return nil
	}
	if err := check(r.Name, "name"); err != nil {
		return err
	}
	if err := check(r.Summary, "summary"); err != nil {
		return err
	}
	if err := check(r.Description, "description"); err != nil {
		return err
	}
	for i, p := range r.Plans {
		if err := check(p.Code, fmt.Sprintf("plans[%d].code", i)); err != nil {
			return err
		}
		if err := check(p.Name, fmt.Sprintf("plans[%d].name", i)); err != nil {
			return err
		}
		if err := check(p.Currency, fmt.Sprintf("plans[%d].currency", i)); err != nil {
			return err
		}
	}
	return nil
}
