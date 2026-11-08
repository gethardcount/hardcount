# Writing style for Hardcount docs

How to write docs in this repo so they are easy to read and still precise.
It is for anyone who writes a new doc or changes an old one, a person or an
AI agent. It applies to every Markdown file: README, guides, runbooks, ADRs,
design docs, CHANGELOG, the release checklist and the files AI agents read
(CLAUDE.md, playbooks).
Comments in code follow the same ideas, with the extra rules in
[Code comments](code-comments.md). The words the dashboard shows follow
[Activity lines and UI words](activity-lines.md).

**In short:** give each page one reader and one job, keep every fact, put the
point first, write one idea per sentence, define words a new reader cannot
know, and keep history behind a link. [Checks](#checks) lists the tools that
enforce this.

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

A page serves its reader both when they are new to Hardcount and when they
know it well. Plain words for the first never cost the second a fact.

## One reader, one job

Every page names its one reader and its one job in its first lines
([Change](#change) rule 1). Results, method, how-to and reference go on
separate pages:

- a how-to holds no measurements;
- a results page holds no steps to reproduce its runs;
- a method page holds no results;
- a reference holds no story of how it came to be.

History, such as an investigation story, goes to a dated record in
`docs/dev/records/`, kept as written. The page that needs its result keeps a
short summary and links the record.

Before you move a fact, `git grep` for its home. If a page already holds it,
delete the copy and link there. If none does, move it verbatim to the page
whose reader needs it. ADR bodies and records never receive moved text.

## Keep

- **Precise technical terms.** Idempotency, at-least-once, RLS, fencing
  token, p99, DLQ. Never replace a precise term with a vague one. Define it
  instead (see [Change](#change) rule 3).
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
3. **Define words a cold reader (someone new to Hardcount and to this page)
   cannot know.** Billing terms (dunning, proration, clawback, credit note,
   prepaid commit) get a short definition on first use in reader-facing
   docs. Project words (parked, catch-up, site-set) are defined once in a
   glossary, or replaced with plain words. Words a user meets go in the
   [glossary](../glossary.md). Words from the code, the database and the
   operations docs go in the [contributor glossary](glossary.md). Write the
   meaning first and keep the term in parentheses, so search and experts
   still find it.
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

Every reader-facing page (README, guides, `docs/` pages a user opens) meets
it, on top of the rules above. Write for a friendly, busy person who has never
heard of Hardcount. The reader should never have to decode a word.

1. **Say what the thing is in one plain line.** "Hardcount is a billing system
   you run yourself" before any feature list.
2. **Say why it exists and who it is for.** One or two lines each, near the
   top.
3. **Give a new idea one everyday comparison.** One, not three. "The quick
   start is a demo car at a showroom: you drive it, you don't build it."
4. **Explain every term on first use, plain meaning first** ([Change](#change)
   rule 3): "a test clock is a pretend clock for test customers". Then use the
   term the same way everywhere after.
5. **End each task step with "You see:"** ([Change](#change) rule 7 makes
   tasks numbered steps). After it comes the real output, the screen, or the
   page the reader lands on. Copy it from a real run, not from memory.

   ```markdown
   1. Run `./quickstart.sh`.
      **You see:** a box with the dashboard address, an email, a password
      and a test API key.
   ```

On these pages, history, ADR and PR numbers, code paths and internal names
all stay out of the reader's path. One "Related" link at the end is enough.
Every number, command, code block and link the reader needs stays (see
[Keep](#keep)).

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
in one short paragraph. Then the full conditions, a summary of the runs, what
it does not show, and any corrections. The steps to reproduce the runs go on
a how-to page. The story of how the result was reached goes in a dated
record. The results doc links both
([One reader, one job](#one-reader-one-job)).

## Checks

- **[Vale](https://vale.sh)** lints prose against [Change](#change) rules 2,
  5 and 6 (`.vale.ini`, rules in `.vale/styles/Hardcount/`). CI comments only
  on lines a PR adds and never fails the build. Run `vale <file>` locally.
- **[lychee](https://lychee.cli.rs)** checks that every internal link and
  `#anchor` resolves. CI fails on a broken one. Run
  `lychee --offline --include-fragments '**/*.md'` locally.

## Changing existing docs safely

- **A PR that rewords, restructures or rewrites a page also fixes its
  facts.** It checks every fact the page keeps against the code, and fixes
  each one that is false. The PR description lists each fact that changed,
  with the code that proves it. Every other difference is wording, proven by
  the fact diff (next item).
- **The fact diff.** This function lists the numbers, inline code,
  code-block lines and link targets of a page. Paste it into bash or zsh,
  then compare the base branch with your branch:

  ```bash
  facts() {
    awk '/^ *[`][`][`]/ { c = !c; next } c { sub(/^ +/, ""); print "code: " $0; next } 1' "$@" |
      grep -oE '^code: .*|`[^`]+`|\]\([^)]*\)|[0-9]([0-9.,:]*[0-9])?%?' | sort
  }
  diff <(git show origin/main:docs/dev/writing-style.md | facts) <(facts docs/dev/writing-style.md)
  ```

  A line that only moved prints nothing. When a PR splits a page, give the
  second `facts` every new page. List every difference in the PR description,
  either as a changed fact with its proof or with a reason, such as a
  repointed link or a deleted history line. To see which lines moved, run
  `git diff --color-moved origin/main -- <files>`. Check that no link or
  `#anchor` broke ([Checks](#checks)).
- **A task check for a runbook that moves money.** Before merge, someone does
  its steps as the page's reader, against the code, and reports any step that
  could charge, refund or bill twice.
- **A cold-reader check for a rewritten or restructured page.** Before merge,
  a reader who has not seen the old page reads the new one, in the role of
  the reader it names. The reader can be a teammate or a fresh AI agent. They
  report each sentence they read twice, each term used before it is defined,
  and whether they could do the job. Their report goes in the PR thread. Each
  finding is fixed, or answered in the thread, before merge.
- **Do not rewrite records.** ADR bodies, old CHANGELOG entries and dated
  audit or benchmark records stay as written. Add a summary or a status
  banner on top instead. The one exception is a link to a page or section
  that moved. Its target changes, and so does the page name in the link's
  own sentence.
