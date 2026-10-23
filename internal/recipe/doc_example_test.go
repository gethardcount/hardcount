package recipe

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// TestRecipeDesignDoc_DunningExampleParses: the recipe design doc's YAML
// example shows the dunning section a recipe author copies. Its keys must
// be ones parseRecipe accepts, and its schedule the one anthropic_style
// ships. The rest of the example is an excerpt of an older format (the
// doc lists where), so only the dunning section is checked.
func TestRecipeDesignDoc_DunningExampleParses(t *testing.T) {
	doc, err := os.ReadFile("../../docs/dev/design/recipes.md")
	if err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(string(doc), "### `internal/recipe/recipes/anthropic_style.yaml` (excerpt)")
	if !ok {
		t.Fatal("recipes.md has no anthropic_style.yaml excerpt")
	}
	_, after, ok = strings.Cut(after, "```yaml\n")
	if !ok {
		t.Fatal("the excerpt has no yaml block")
	}
	block, _, _ := strings.Cut(after, "```")
	section := topLevelSection(block, "dunning")
	if section == "" {
		t.Fatal("the excerpt has no dunning section")
	}

	base, _, _ := strings.Cut(minimalRecipe, "dunning:\n")
	got, err := parseRecipe([]byte(base + section))
	if err != nil {
		t.Fatalf("the doc's dunning section does not parse: %v\n%s", err, section)
	}
	shipped, err := os.ReadFile("recipes/anthropic_style.yaml")
	if err != nil {
		t.Fatal(err)
	}
	want, err := parseRecipe([]byte(base + topLevelSection(string(shipped), "dunning")))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.DunningPolicy, want.DunningPolicy) {
		t.Errorf("the doc's dunning policy is %+v, anthropic_style ships %+v", *got.DunningPolicy, *want.DunningPolicy)
	}
}

// topLevelSection returns the YAML lines from "key:" at column zero up to
// the next top-level key.
func topLevelSection(yaml, key string) string {
	var b strings.Builder
	in := false
	for _, line := range strings.SplitAfter(yaml, "\n") {
		top := line != "" && line[0] != ' ' && line[0] != '#' && line[0] != '\n'
		if top {
			in = strings.HasPrefix(line, key+":")
		}
		if in {
			b.WriteString(line)
		}
	}
	return b.String()
}
