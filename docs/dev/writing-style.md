# Writing style for Hardcount docs

How to write docs, changelog entries and pull request (PR) text. It covers every Markdown file in the repo.

Comments also follow [Code comments](code-comments.md), and dashboard words follow [Activity lines and UI words](activity-lines.md). Older text does not all follow these rules yet.

## The rules

1. **Point first.** Open a page or section with what it is, who it is for and what the reader gets. State the rule or result, then the reason.
2. **One idea per sentence.** A sentence over 35 words needs a reason. Break up chains of dashes, semicolons and parentheses, and rewrite any sentence you read twice.
3. **Define a word the reader cannot know,** in a few plain words on first use, or link the [product glossary](../glossary.md) (billing words) or [contributor glossary](glossary.md) (code words). Keep the precise term beside its meaning, so search finds it. Use one word for one idea.
4. **Describe today, and keep history behind a link.** Leave out incident stories, PR numbers (a CHANGELOG entry links its PR), "used to" and review rounds. If history is the reason for a rule, state the reason in the present tense and point to the decision record ("See ADR-041"). The text must make sense without it. An investigation's story goes in a dated file in `docs/dev/records/`.
<!-- vale off -->
5. **Write global English.** No idioms such as "bolted on" or "hairball" ([the list](../../.vale/styles/Hardcount/Idioms.yml)), and no stacked "not X, but Y" turns.
6. **Keep emphasis quiet.** Bold only marks the words a reader scans for, and capitals never mark emphasis. Leave out defensive words such as "honestly" ([the list](../../.vale/styles/Hardcount/Defensive.yml)): the evidence makes the point.
<!-- vale on -->
7. **Fit the shape to the job.** A task is numbered steps, reference is a table, and a concept is short paragraphs.
8. **No walls.** A paragraph over about 120 words, or a table cell over about 40, becomes a list, table or sub-section.
9. **On a page for users,** a new idea gets one everyday comparison, and each step ends with "You see:" and its real output. ADR numbers, code paths and internal names stay out.

## Docs change with the code

A change in behavior updates its docs in the same PR. Check these too:

- `CHANGELOG.md`, for every change a user can see.
- The [release checklist](release-checklist.md), when a live Stripe, email or PDF check should show something new.
- A new ADR in [`docs/adr/`](../adr/README.md), for a decision someone may want to reopen. Nothing checks its number: list `docs/adr/` on `origin/main` and every branch, then take the next free one.
- The [roadmap](../roadmap.md), when an item ships, or before you drop one. Maintainers edit it: an outside PR names the line in its description.

## Shapes for common docs

- **CHANGELOG entry.** At most about 50 words: a bold outcome, who is affected, and any action to take. Add the PR link once the PR is open. A fix to an amount says which way it was wrong, and its PR gives a query that finds the records it affected.
- **PR description.** Fill in the template. How the change works, how it was tested and its history go here, not in the CHANGELOG.
- **ADR.** Follow the [template](../adr/TEMPLATE.md), and keep its Summary current.
- **Checklist item.** One action and the result to see, on one line.
- **Runbook section.** Symptom, Why, Check, Fix, Verify. No procedure inside a table cell.

## One reader, one job

A page has one job and one reader: an evaluator who may not know billing, an integrator, an operator mid-incident, or a contributor. All are technical and busy, and many read English as a second language.

Results, method, how-to and reference go on separate pages. A fact has one home: before you repeat one, `git grep` for it and link its page.

## Shorter, with nothing lost

Cut words, not facts. Keep each number, command, code block and link the reader uses. A results page keeps its conditions, what the test does not show, and any withdrawn claim.

**Nothing a maintainer needs is dropped when text is cut.** It moves to where it belongs: a test, a short comment, the page for that flow, or a new ADR.

## Checks

[Vale](https://vale.sh) annotates the lines a PR adds for a sentence over 35 words, a second em dash, or a listed idiom or defensive word, and never fails the build. [lychee](https://lychee.cli.rs) fails the `doc-links` job on a broken internal link or `#anchor`. Review checks the rest. Commands: [What CI runs on your PR](../../CONTRIBUTING.md#what-ci-runs-on-your-pr).

## Changing existing text

- **A PR that only rewords comments changes no code and no fact.** A wrong comment may mean wrong code, so one you find during other work gets its own PR or issue.
- **A PR that rewrites a doc also fixes any false statement it finds,** and lists each fix in its description. A wrong doc never means wrong code.
- **Records stay as written:** ADR files below their Summary, released CHANGELOG entries and the files in `docs/dev/records/`. Add a summary or status line on top. Only a link to a page that moved may change, and an ADR amendment goes at the end.
- Before merge, someone who has not seen the old text reads the new comments or page as its reader would.
- **A runbook that moves money gets a dry run before merge.** Someone follows its steps against the code and reports any that could charge, refund or bill twice.
