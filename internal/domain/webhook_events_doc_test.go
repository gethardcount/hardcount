package domain

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// webhookEventsDoc is the reader's catalog of outbound webhook events. Each
// family of events is a "## … `data.<resource>`" section, and each event a
// "### `<event>`" section under it, with an "**Also in `data`:**" line and
// one example message in a ```json fence.
const webhookEventsDoc = "../../docs/api/webhook-events.md"

// docEvent is one "### `<event>`" section of the events page.
type docEvent struct {
	name     string
	resource string   // the resource of the "##" family heading above it
	extras   []string // the keys named on its "Also in `data`:" line, sorted
	hasAlso  bool     // whether the section has an "Also in `data`:" line
	example  []string // the lines inside its ```json fence
	fences   int      // how many ```json fences the section has
	fenceAt  int      // the index of the first fence's opening line
}

var (
	docFamilyHeading = regexp.MustCompile("^## .*`data\\.([a-z_]+)`")
	docEventHeading  = regexp.MustCompile("^### `([a-z_.]+)`$")
	docBacktick      = regexp.MustCompile("`([a-z_.]+)`")
)

// parseWebhookEventsDoc reads the events page into its event sections, in
// page order, and returns the page's lines as well.
func parseWebhookEventsDoc(t *testing.T) ([]docEvent, []string) {
	t.Helper()
	raw, err := os.ReadFile(webhookEventsDoc)
	if err != nil {
		t.Fatalf("read %s: %v", webhookEventsDoc, err)
	}
	lines := strings.Split(string(raw), "\n")
	var (
		events   []docEvent
		resource string
		cur      *docEvent
		inFence  bool
		inAlso   bool
	)
	for i, line := range lines {
		if inFence {
			if line == "```" {
				inFence = false
				continue
			}
			if cur != nil && cur.fences == 1 {
				cur.example = append(cur.example, line)
			}
			continue
		}
		if m := docFamilyHeading.FindStringSubmatch(line); m != nil {
			resource, cur, inAlso = m[1], nil, false
			continue
		}
		if strings.HasPrefix(line, "## ") {
			resource, cur, inAlso = "", nil, false
			continue
		}
		if m := docEventHeading.FindStringSubmatch(line); m != nil {
			events = append(events, docEvent{name: m[1], resource: resource})
			cur, inAlso = &events[len(events)-1], false
			continue
		}
		if strings.HasPrefix(line, "#") {
			cur, inAlso = nil, false
			continue
		}
		if cur == nil {
			continue
		}
		if line == "```json" {
			cur.fences++
			if cur.fences == 1 {
				cur.fenceAt = i
			}
			inFence = true
			continue
		}
		if idx := strings.Index(line, "**Also in `data`:**"); idx >= 0 {
			cur.hasAlso, inAlso = true, true
			line = line[idx+len("**Also in `data`:**"):]
		}
		if inAlso {
			if strings.TrimSpace(line) == "" {
				inAlso = false
				continue
			}
			for _, m := range docBacktick.FindAllStringSubmatch(line, -1) {
				cur.extras = append(cur.extras, m[1])
			}
		}
	}
	for i := range events {
		sort.Strings(events[i].extras)
	}
	return events, lines
}

// webhookEventsDocExample is the example message the page must show for an
// event: the body Hardcount sends, with the golden data, indented.
func webhookEventsDocExample(t *testing.T, eventType string) []string {
	t.Helper()
	msg := map[string]any{
		"id":         "hc_whevt_golden",
		"event_type": eventType,
		"created_at": goldenAt.Format(time.RFC3339),
		"data":       goldenWebhookData(t, eventType),
	}
	b, err := json.MarshalIndent(msg, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(string(b), "\n")
}

// TestWebhookEventsDoc_OneSectionPerEvent: docs/api/webhook-events.md has
// one section for every event in the catalog, and none for an event outside
// it.
func TestWebhookEventsDoc_OneSectionPerEvent(t *testing.T) {
	events, _ := parseWebhookEventsDoc(t)
	seen := map[string]int{}
	for _, ev := range events {
		seen[ev.name]++
	}
	for _, v := range sortedKeys(KnownWebhookEventTypes) {
		switch seen[v] {
		case 0:
			t.Errorf("%s has no section for %q", webhookEventsDoc, v)
		case 1:
		default:
			t.Errorf("%s has %d sections for %q", webhookEventsDoc, seen[v], v)
		}
	}
	for _, v := range sortedKeys(seen) {
		if !KnownWebhookEventTypes[v] {
			t.Errorf("%s has a section for %q, which is not in KnownWebhookEventTypes", webhookEventsDoc, v)
		}
	}
}

// TestWebhookEventsDoc_ResourceAndExtras: each event's section sits under
// the heading of its family's resource ("## … `data.invoice`"), and its
// "Also in `data`:" line names exactly the keys WebhookEventExtras gives it,
// or "nothing".
func TestWebhookEventsDoc_ResourceAndExtras(t *testing.T) {
	events, _ := parseWebhookEventsDoc(t)
	for _, ev := range events {
		if !KnownWebhookEventTypes[ev.name] {
			continue // reported by TestWebhookEventsDoc_OneSectionPerEvent
		}
		if want := WebhookEventResource(ev.name); ev.resource != want {
			t.Errorf("%s: %s is under `data.%s`, but its data carries `%s`", webhookEventsDoc, ev.name, ev.resource, want)
		}
		if !ev.hasAlso {
			t.Errorf("%s: %s has no \"**Also in `data`:**\" line", webhookEventsDoc, ev.name)
			continue
		}
		want := append([]string(nil), WebhookEventExtras[ev.name]...)
		sort.Strings(want)
		if strings.Join(ev.extras, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %s lists %v beside the resource, the code sends %v", webhookEventsDoc, ev.name, ev.extras, want)
		}
	}
}

// TestWebhookEventsDoc_ExamplesAreTheRealShape: each event's example is the
// message Hardcount sends for it, built from the same fixed resources as the
// payload goldens (TestWebhookPayloadGolden). With -update it rewrites the
// examples, so a payload change and its docs land in one diff.
func TestWebhookEventsDoc_ExamplesAreTheRealShape(t *testing.T) {
	events, lines := parseWebhookEventsDoc(t)
	type rewrite struct {
		at, n int // the fence's opening line and how many lines it holds
		want  []string
	}
	var rewrites []rewrite
	for _, ev := range events {
		if !KnownWebhookEventTypes[ev.name] {
			continue
		}
		if ev.fences != 1 {
			t.Errorf("%s: %s has %d ```json examples, want 1", webhookEventsDoc, ev.name, ev.fences)
			continue
		}
		want := webhookEventsDocExample(t, ev.name)
		if strings.Join(ev.example, "\n") == strings.Join(want, "\n") {
			continue
		}
		if *updateGolden {
			rewrites = append(rewrites, rewrite{at: ev.fenceAt, n: len(ev.example), want: want})
			continue
		}
		t.Errorf("%s: the %s example is not the message the code sends (run go test ./internal/domain -update and review the diff)\n--- want\n%s",
			webhookEventsDoc, ev.name, strings.Join(want, "\n"))
	}
	if len(rewrites) == 0 {
		return
	}
	// Apply from the bottom up so earlier line numbers stay valid.
	sort.Slice(rewrites, func(i, j int) bool { return rewrites[i].at > rewrites[j].at })
	for _, rw := range rewrites {
		body := lines[rw.at+1+rw.n:]
		lines = append(append(lines[:rw.at+1:rw.at+1], rw.want...), body...)
	}
	if err := os.WriteFile(webhookEventsDoc, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Printf("rewrote %d examples in %s\n", len(rewrites), webhookEventsDoc)
}
