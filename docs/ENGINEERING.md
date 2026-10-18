# How Velox is engineered

Velox moves money, so these are the rules the code is built to. Each one links to where you can see it.

## Money

- **A billing period is billed once.** Every cycle close locks the subscription row and checks that its period has not moved ([ADR-115](adr/115-one-closer-for-the-billing-period.md)). Behind that, a unique index in the database refuses a second live cycle invoice for the same period, so a crash or two leaders racing cannot bill it twice. Other money writes use their own dedup index or a conditional update. Card charges also send Stripe a stable idempotency key. [The run that shows it](benchmarks/failure-correctness.md)
- **Constraints first, then a checker.** Database constraints block the bad states they can express. For rules that span rows, a read-only checker (velox-doctor, 30 checks) finds bad rows after they are written. CI runs it over the golden-path demo data, and the clock soak test runs it after every billing close. [`cmd/velox-doctor`](../cmd/velox-doctor)
- **Fail loudly.** If a read that a bill needs fails, the bill stops with an error; it does not bill a default. A subscription that keeps failing is parked for an operator. One exception, in test mode only: if a test clock cannot be read, billing uses the current time and logs a warning. [ADR-119](adr/119-billing-failure-record-keyed-to-the-due-instant.md)
- **For card charges, Stripe's record decides what was paid.** A charge with an unclear result is held until a webhook or a Stripe search names it, or an operator writes the invoice off. An operator can also record a payment made outside Stripe. [Card payment failure handling](payments.md) lists the cases and the known gaps. [ADR-107](adr/107-unknown-is-terminal-until-a-human.md)

## Simplicity

- **One Postgres.** All billing data lives there; Redis is used for rate limiting only. No queue broker, warehouse or workflow engine until a measurement says Postgres is not enough. [How much traffic Velox handles](scale.md)
- **Prefer one path for each job.** Here, a second path for the same job is where bugs have hidden; ADR-074 to ADR-077 removed one. [Decision records](adr/README.md)
- **The smallest correct change first.** An extra mechanism needs a reason: the defect it prevents, written in its ADR or PR. [Decision records](adr/README.md)

## Proof

- **Money paths are tested against a real database.** The money-path playbook asks for tests against real Postgres. It also asks you to break the fix on purpose once, to show the test then fails. A few cases are exempt, as the playbook lists. [Money-path playbook](dev/money-path-robustness-playbook.md)
- **Benchmarks come with their method.** Each published benchmark states its method, its hardware and what the run does not show, next to the number. [Sustained throughput](benchmarks/sustained-throughput.md)
- **Money changes get the full process.** Changes to money or a billing state machine (invoices, payments, credits, dunning, subscriptions, tax) follow the money-path playbook. Other changes get a normal review. [Money-path playbook](dev/money-path-robustness-playbook.md)

## Going deeper

- [The evidence behind these](engineering-evidence.md): the constraint, the measurements, the decisions that were reversed, and the defects the benchmarks found in Velox.
- [Architecture](architecture.md): package layout and design rules.
- [Decision records](adr/README.md): one decision per file; newer ones list the alternatives considered.
