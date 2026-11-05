# Activity lines and UI words

The rules for the words the dashboard shows. They are for developers who add
or change that text, in the backend or in the dashboard. Activity lines have
their own rules below. The word list after them applies to every page a user
or an end customer sees.

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
     and then link it (rule 8, "Link what the row's own data names").
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

## Words to keep out of UI text

The dashboard and the pages an end customer sees use the reader's words, not
the code's.

| Do not write | Write |
|---|---|
| tenant | account |
| catchup, catch-up running | what is happening: "Billing the skipped days…" |
| `event_name` or any field name | what it means: "The name your app sends with each usage event, for example `api_calls`" |
| Configure … | what the page is for: "Your business details, invoice defaults, tax and payment setup" |
