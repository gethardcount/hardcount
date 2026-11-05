# Code comments

These rules cover comments in Go code. The reader is a contributor, a
maintainer or an AI agent with the code open, so a comment adds only what the
code cannot say. The [writing style](writing-style.md) rules still apply:
one idea per sentence, plain words and quiet emphasis.

1. **Say why, not what.** Explain the reason, the rule the code keeps, or the
   trap it avoids. Do not repeat what the next line does. If a better name
   would make the comment unnecessary, rename instead
   ([Google Go style guide](https://google.github.io/styleguide/go/guide#clarity)).
2. **Doc comments follow Go's conventions**
   ([Go Doc Comments](https://go.dev/doc/comment)).
   - Every exported name has one. It is a full sentence that starts with the
     name.
   - It says what the function returns or does, and what a caller must know:
     preconditions, errors, whether it runs on the caller's transaction, and
     whether it is safe to call concurrently.
   - How the function works goes in comments inside its body.
   - Each package has one package comment (`// Package billing …`), in
     `doc.go` or the package's main file.
3. **Rule first, then the reason.** Write the rule in one sentence and the
   reason in the next: "Allocate the number on the caller's transaction.
   Otherwise a second connection waits on the row this transaction holds."
   When a sentence says "so" or "this", the reader must see what it points to
   on the first read.
4. **Keep it short.** Most comments are one to three lines. A doc comment over
   about 15 lines, or a comment inside a function over about 8, needs a
   reason: usually several rules that a caller must all know. Design
   discussion, rejected options and future plans go in an ADR, and the comment
   links to it.
5. **Describe today's code, and keep history behind a link.** Leave out
   incident stories, "used to", "pre-fix", corrections, audit dates and review
   rounds. When history is the reason for a rule, state the reason in the
   present tense and add a pointer: "(See ADR-107.)" The comment must make
   sense without opening the link.
6. **State rules, not counts or call sites.** "Every service guard calls this"
   stays true. "The seven call sites" goes stale the day an eighth is added.
   Name a caller, file or line only when the reader needs to go there.
7. **Write plainly.**
   - No capitals for emphasis (NOT, NEVER, MUST). Plain "must" and "never",
     with the reason, carry the weight.
   - Break chains of dashes and semicolons into sentences.
   - Define a project word on first use (parked, lane, catch-up), or use plain
     words. Spell identifiers exactly as the code does.
   - Wrap at about 80 columns, as most of the code already does.
8. **A wrong comment is a bug.** When you change code, update or delete the
   comments that describe it in the same commit. A false comment you find
   while doing other work gets its own PR or an issue. It never rides inside a
   clarity change.

Keep the markers that tools read: `//go:` directives, and
`//nolint:<linter> // <reason>` waivers (golangci-lint requires the reason).

## Before and after

A "so" whose subject is not said (`cmd/hardcount/main.go`):

```go
// Before
// A period is closed on the first billing tick after its end plus the
// grace, so the operator sees when invoices will actually be built.
slog.Info("invoice grace period", "invoice_grace_period", cfg.InvoiceGracePeriod, "billing_interval", billingInterval)

// After
// Log the grace and the billing interval together. A period closes on the
// first billing tick after its end plus the grace, so an operator needs both
// to know when invoices are built.
```

History and capitals in place of the reason (same file):

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
// checks. The scheduler skips a worker whose service is nil, which tests
// rely on. A nil check here would hide a wiring bug by turning off a
// money-path worker without a word.
```

A doc comment that grew into a design record. The one on
`domain.PaymentBlocksAction` had 83 lines: a correction story, a count of call
sites and the design of a superseded ADR. A caller needs these 16:

```go
// PaymentBlocksAction reports whether the invoice's payment state blocks an
// operator action, and what to tell the operator. It is the one statement of
// this rule: change it here, not at the call sites.
//
// There are three cases:
//   - No payment in flight: nothing is blocked here. The caller's own status
//     rules (paid, voided, draft) still apply, because they differ per action.
//   - Parked (payment_status 'unknown' with no PaymentIntent ID): Stripe's
//     answer named no payment, and Hardcount checks the customer's payments
//     at Stripe within a day. Only mark-uncollectible is allowed, for a
//     customer who paid another way. Every other action is refused, and the
//     message says what Hardcount does next.
//   - In flight with a known PaymentIntent: the charge is resolving, so every
//     action is refused and the operator is told to wait.
//
// See ADR-107 and ADR-127 for the parked state.
```

## Changing existing comments

- **One package per PR, comments only.** No code changes, so the tests and
  lint show that no behaviour changed. Small PRs keep conflicts with other
  work cheap. Small sibling packages, under about 300 comment lines each,
  can share a PR, and a file with over about 1,000 comment lines gets its
  own.
- **Prove it.** This prints every changed line that is not a comment or blank,
  and must print nothing:

  ```bash
  git diff -U0 origin/main -- '*.go' | grep -E '^[+-][^+-]' | grep -vE '^[+-][[:space:]]*(//.*)?$'
  ```

  A comment at the end of a code line shows up here. Check each one by hand.
- **Truth and clarity stay apart.** A rewording PR keeps every fact. A
  comment that disagrees with the code is fixed in a truth PR that re-checks
  the claim against the code.
