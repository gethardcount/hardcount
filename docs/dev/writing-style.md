# Writing style for Hardcount docs

How to write docs in this repo so they are easy to read and still precise.
Use it when you write a new doc and when you touch an old one. It applies to
every Markdown file: README, guides, runbooks, ADRs, design docs, CHANGELOG,
the release checklist and the files AI agents read (CLAUDE.md, playbooks).
Comments in Go code follow the same ideas, with the extra rules in
[Code comments](#code-comments).

**In short:** keep every fact, put the point first, write one idea per
sentence, define words a new reader cannot know, and keep history behind a
link. [Checks](#checks) lists the tools that enforce this.

## Who reads our docs

All our readers are technical. Many read English as a second language, and
most are short of time.

| Reader | What they need | Main docs |
|---|---|---|
| Evaluator: an engineer or founder deciding if Hardcount fits, usually not a billing expert | What it is, who it is for, how to try it, in 5 minutes | README, self-host, architecture |
| Integrator: a developer wiring a product to the API | Exact requests, status codes, copy-paste examples | API docs, webhooks, integrations |
| Operator: an SRE running Hardcount, sometimes during an incident | Symptom, then command, then how to check the fix | self-host, deploy, `docs/ops/` |
| Contributor: an engineer making a first change | Where the code is, the rules, and why | CONTRIBUTING, architecture, ADRs |
| Reviewer: a senior engineer skimming for 2 minutes | The result, the evidence, the reasoning | ENGINEERING.md, benchmarks |
| AI agent working in the repo | Rules it can extract fast and follow exactly | CLAUDE.md, playbooks |
| Maintainer, months later | A decision or a change, found fast | ADRs, CHANGELOG |

The effort a reader spends should go into the ideas, never into untangling
the sentences.

## Keep

- **Precise technical terms.** Idempotency, at-least-once, RLS, fencing
  token, p99, DLQ. Never replace a precise term with a vague one. Define it
  instead (see rule 3).
- **Every number, command, code block, link and table value.**
- **The reasoning.** Engineers come for the why. Keep it; put it after the
  what.
- **The limits.** "What this does not show" sections, negative controls and
  withdrawn claims build trust. Keep them, in one line where one line is
  enough.

## Change

1. **Point first.** The first one to three lines of a doc or section say
   what it is, who it is for, and what the reader gets. History comes after,
   or behind a link.
2. **One idea per sentence.** Aim for 15 to 22 words on average. A sentence
   over 35 words needs a reason. Most long sentences are chains of dashes,
   semicolons and parentheses: break the chain first.
3. **Define words a cold reader cannot know.** Billing terms (dunning,
   proration, clawback, credit note, prepaid commit) get a short definition
   on first use in reader-facing docs. Project words (lane, parked, catch-up,
   site-set) are defined once in a glossary or replaced with plain words.
   Write the meaning first and keep the term in parentheses, so search and
   experts still find it.
4. **Keep history out of the reader's path.** PR numbers, incident stories
   and ADR numbers used as the *reason* for a rule go behind a link: "See
   ADR-041", not "per ADR-041, which removed the fallback in #312".
<!-- vale off -->
5. **Write global English.** No idioms ("bolted on", "gold-plate",
   "hairball"). No stacked "not X, but Y" reversals. A careful reader whose
   first language is not English should get each sentence in one pass.
6. **Keep emphasis quiet.** Bold is for scan anchors. Do not shout with
   ALL-CAPS or with bold in running text. Leave out defensive words
   ("honestly", "this is not a lie"); the evidence makes the point.
<!-- vale on -->
7. **Fit the shape to the job.** Tasks are numbered steps. Reference is a
   table. Concepts are short paragraphs, plus a diagram where it helps.
   Decisions follow the ADR template.
8. **No walls.** A paragraph over about 120 words, or a table cell over about
   40, becomes a list, a table or a sub-section.

**The one-pass test.** Read the sentence once, at normal speed, as a new
reader would. If you had to read it twice, or hold a parenthesis in your head
until the end, rewrite it.

## The first-time reader bar

Set on 2026-10-17. Every reader-facing page (README, guides, `docs/` pages a
user opens) meets it, on top of the rules above. Write for a friendly, busy
person who has never heard of Hardcount. The reader should never have to decode a
word.

1. **Say what the thing is in one plain line.** "Hardcount is a billing system
   you run yourself" before any feature list.
2. **Say why it exists and who it is for.** One or two lines each, near the
   top.
3. **Give a new idea one everyday comparison.** One, not three. "The quick
   start is a demo car at a showroom: you drive it, you don't build it."
4. **Explain every term on first use, plain meaning first** (rule 3): "a
   test clock is a pretend clock for test customers". Then use the term the
   same way everywhere after.
5. **End each task step with "You see:"** (rule 7 makes tasks numbered
   steps). After it comes the real output, the screen, or the page the
   reader lands on. Copy it from a real run, not from memory.

   ```markdown
   1. Run `./quickstart.sh`.
      **You see:** a box with the dashboard address, an email, a password
      and a test API key.
   ```

On these pages, history, ADR and PR numbers, code paths and internal names
all stay out of the reader's path. One "Related" link at the end is enough.
Every number, command, code block and link the reader needs stays (see
[Keep](#keep)).

### Words to keep out of UI text

The dashboard and the pages an end customer sees use the reader's words, not
the code's.

| Do not write | Write |
|---|---|
| tenant | account |
| catchup, catch-up running | what is happening: "Billing the skipped days…" |
| `event_name` or any field name | what it means: "The name your app sends with each usage event, for example `api_calls`" |
| Configure … | what the page is for: "Your business details, invoice defaults, tax and payment setup" |

### Before and after

The Test clocks page intro.

Before:

> A test clock freezes 'now' for the subscriptions you pin to it. Advance the
> clock to fast-forward billing without waiting for real time to pass.

After:

> A test clock is a pretend clock for test customers. Create a clock, add a
> new customer on it, then move the clock forward to see renewals, invoices
> and payment retries happen in minutes.

The new text says what a test clock is, uses no undefined words ("pin",
"freezes 'now'"), and is true: a clock holds customers, not subscriptions.

## Activity lines

An activity line is one row on an activity card: the invoice and subscription
Activity cards, the Dunning page's recovery timeline and the customer's Sent
emails card. The backend builds every word of a line. The dashboard formats
money and times and lays the row out. Use these rules when you add or change a
line. The decision behind them is
[ADR-128](../adr/128-activity-line-standard.md).

A line has up to three parts:

```
●  Payment failed · $24.73                                    10:13 AM
   Card declined: insufficient funds          <- problem line
   Automatic · Recorded Oct 18, 8:14 PM       <- meta line
```

1. **One event, one wording.** An event has the same title on every page it
   appears on, and the audit log's headline uses the same verb. Two pages
   never use two phrasings for one event.
2. **Title: object plus past participle, sentence case, no values.**
   - "Invoice finalized", "Payment failed", "Credit note issued".
   - No dashes, no brackets and no actor in the title.
   - Name another object's number only when it is not the page's own entity,
     and then link it (rule 8).
   - Two outcomes of one event share one title. The difference goes in the
     problem line or the meta line.
3. **Money only when the event moves money, asks for it, or cancels money
   owed** (a void or a write-off).
   - It goes right after the title, in the document's own currency.
   - The dashboard's one money formatter prints it: `$1,234.50`, `CA$12.00`.
     Never cents, never `¢`, never `12.00 CAD`.
   - A title's amount always means the same thing. A credit note row always
     shows the credit note's total; the meta line splits it into its parts.
4. **Who did it, on every line, from a fixed set.** The actor is the last
   part of the meta line, before any Recorded part:
   - a person: `by <name>`, or `by <email>` when there is no name;
   - an API key: `by API key <key name>`;
   - the customer: `Customer, on the payment page` or
     `Customer, in the portal`;
   - anything Hardcount did on its own: `Automatic`.

   Never `system`, `api_key`, `operator`, "an operator" or "by Hardcount". The
   actor comes from a fact written with the row. A row with no such fact
   shows no actor; only legacy rows lack one.
5. **Plain words.** No enum codes, table names, Go errors, meter keys or
   provider wrappers such as `payment failed: …`. Test: would a finance
   person who has never seen the code understand it?
   - Write decline reasons about the cardholder, not to them: "Card declined:
     insufficient funds", not "Your card has insufficient funds."
   - "Uncollectible" is the one word for an invoice written off as bad debt.
6. **One clock rule.**
   - A line is dated on the entity's calendar, at the instant the event
     happened. For an automatic event, that is the instant the contract names
     (renewal, trial end, retry slot), never the instant a clock was moved.
   - On a test clock, the meta line ends with `· Recorded <real date and
     time>`. "Recorded" is the only word for the real-world time.
   - Nothing else differs between a test-clock line and a real-clock line.
7. **Four tones, one meaning each.** The words carry the meaning; the dot only
   repeats it.

   | Tone | Colour | Meaning |
   |---|---|---|
   | Neutral | grey | Something happened |
   | Success | green | Money arrived, or was returned as intended |
   | Warning | amber | Needs a look, or money will not be collected |
   | Failure | red | An attempt failed |

   A failure or warning row has a problem line that says what went wrong or
   what it means, unless the title already says all of it ("Payment retries
   ran out"). Leave out a problem line that only repeats the title.
8. **Link what the row's own data names:** an invoice, a credit note or a
   subscription whose id the row carries, and only while it still exists. A
   deleted object stays plain text. Never link from a guess, and never to a
   payment-recovery run: the Dunning page cannot open on one run.

## Shapes for common docs

**ADR.** Follow [`docs/adr/TEMPLATE.md`](../adr/TEMPLATE.md). The Summary at
the top says what was decided and why in two to five plain sentences. When a
later change amends the decision, update the Summary's current rule and add
the amendment at the end, dated.

**CHANGELOG entry.** One entry per user-visible change, at most about 50
words:

```markdown
- **Every dunning retry now counts against `max_retry_attempts`.** During a
  leader handover one charge could go uncounted, so a run could exceed its
  retry limit. No charge was doubled. ([#NNN])
```

A bold outcome, then who is affected and what changes for them, then any
action they must take, then a link. The mechanism, the tests and the history
go in the PR description or the ADR.

**Checklist item** (for example in the release checklist). One action and
the one result to see, on one line:

```markdown
- [ ] Refund: a credit note with "Refund to card" → `refund_status=succeeded`,
  and the refund shows in the Stripe dashboard.
```

**Runbook section.** Symptom, Why, Check (commands), Fix, Verify. A procedure
never lives inside a table cell.

**Results doc (benchmark, drill).** Open with the result and its conditions
in one short paragraph. Then the setup, the runs, what it does not show, and
any corrections. The story of how the result was reached goes in an appendix.

## Code comments

These rules cover comments in Go code. The reader is a contributor, a
maintainer or an AI agent with the code open, so a comment adds only what the
code cannot say. The rules above still apply: one idea per
sentence, plain words and quiet emphasis.

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

### Before and after

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
`domain.PaymentBlocksAction` is 83 lines: a correction story, a count of call
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

### Changing existing comments

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
- **Truth and clarity stay apart**, as for docs (see
  [Changing existing docs safely](#changing-existing-docs-safely)). A
  rewording PR keeps every fact. A comment that disagrees with the code is
  fixed in a truth PR that re-checks the claim against the code.

## Checks

- **[Vale](https://vale.sh)** lints prose against rules 2, 5 and 6
  (`.vale.ini`, rules in `.vale/styles/Hardcount/`). CI comments only on lines a
  PR adds and never fails the build. Run `vale <file>` locally.
- **[lychee](https://lychee.cli.rs)** checks that every internal link and
  `#anchor` resolves. CI fails on a broken one. Run
  `lychee --offline --include-fragments '**/*.md'` locally.

## Changing existing docs safely

- **Clarity and truth go in separate PRs.** A clarity PR changes no facts. A
  truth PR fixes a doc that disagrees with the code, and re-derives each
  claim from the code.
- **Prove a clarity PR changed no facts.** Compare the numbers, code, code
  blocks and link targets of each changed file against the base branch, and
  explain every difference. Check that no link or `#anchor` broke.
- **Do not rewrite records.** ADR bodies, old CHANGELOG entries and dated
  audit or benchmark records stay as written. Add a summary or a status
  banner on top instead.
