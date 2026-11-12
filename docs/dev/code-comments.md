# Code comments

How to write a comment in Go, TypeScript, shell scripts, Makefiles, Dockerfiles and CI YAML.

A comment has two readers. A developer new to the code understands it on first read, without opening another file. A maintainer finds the rules and traps a change could break.

The [writing rules](writing-style.md#the-rules) apply too, above all: no history, no capitals for emphasis. Only review checks this page. Older comments do not all follow it.

## The rules

1. **Say why, not what.** Give the reason, the rule the code keeps or the trap it avoids.
2. **A long comment means the code needs work.** If a guard, fallback or workaround takes many lines to explain, remove the second path or fix the cause, and most of the comment goes too.
3. **Keep it short.** Most comments are one to three lines, wrapped at about 80 columns. A doc comment over about 15 lines, or a comment inside a function over about 8, needs a reason. Design discussion, rejected options and plans go in a decision record (an ADR, `docs/adr/`). When you cut, [nothing a maintainer needs is dropped](writing-style.md#shorter-with-nothing-lost).
4. **A doc comment tells the caller what they must know:** what the function returns or does, its preconditions, its errors, whether it uses the caller's transaction and whether concurrent calls are safe. Every exported name and package has one, in [Go Doc Comments](https://go.dev/doc/comment) form.
5. **In TypeScript,** an exported function, hook or component gets a `/** */` comment ([TSDoc](https://tsdoc.org)) when its name and types do not say enough.
6. **State rules, not counts or call sites.** "Every service guard calls this" stays true. "The seven call sites" is wrong once an eighth is added.
7. **A wrong comment is a bug.** Change the comment with the code: update or delete it in the same commit.
8. **Leave tool markers as they are:** `//go:` directives, `eslint-disable`, `@ts-expect-error`, `# shellcheck` and `//nolint`.

To reword or fix existing comments, see [Changing existing text](writing-style.md#changing-existing-text).

## Example

History and capitals where the reason belongs (adapted from `cmd/hardcount/main.go`):

```go
// Before
// Every service below is constructed unconditionally by api.NewServer —
// a nil can only mean a broken refactor. Wire unconditionally: the
// scheduler's own tick-time nil-guards are a seam for TEST
// configurations, and hiding a wiring bug behind them would skip a
// money-path worker silently, forever. (Seven dead nil-guards here did
// exactly that in theory until the 2026-08-11 module audit.)

// After
// api.NewServer always builds these services, so wire them without nil
// checks. A nil check here would hide a wiring bug by turning off a
// money-path worker without a word.
```
