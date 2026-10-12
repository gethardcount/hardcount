# How Velox is engineered

Velox moves money, so these are the rules the code is built to. Each one links to where you can see it.

## Money

- **Every money effect happens exactly once.** The database enforces it, not application code. [The run that shows it](benchmarks/failure-correctness.md)
- **A state that should not exist cannot be stored.** Constraints come first. A checker covers the rules that span rows. [`cmd/velox-doctor`](../cmd/velox-doctor)
- **Fail loudly, never guess.** A failed read stops the bill; it does not bill a default. [Failure modes](failure-modes/README.md)
- **The payment provider's record decides what was paid.** An unclear charge is held until it is known. [ADR-107](adr/107-unknown-is-terminal-until-a-human.md)

## Simplicity

- **One Postgres.** All billing data lives there; Redis is used for rate limiting only. No queue, warehouse or workflow engine until a measurement says Postgres is not enough. [Will it take our volume?](scale.md)
- **One way to do each thing.** A second path for the same job is where bugs hide. [Architecture](architecture.md)
- **The smallest correct change first.** Every extra mechanism names the defect it prevents. [Decision records](adr/README.md)

## Proof

- **Money paths are tested against a real database.** The fix is then broken on purpose, to prove the test catches it. [Money-path playbook](dev/money-path-robustness-playbook.md)
- **Claims come with their measurement.** Method, hardware and what the run does not show are published with the number. [Sustained throughput](benchmarks/sustained-throughput.md)
- **Rigor follows reversibility.** Money, sent invoices, the schema and the public API get the full process. The rest gets a normal review. [Money-path playbook](dev/money-path-robustness-playbook.md)

## Going deeper

- [The evidence behind these](engineering-evidence.md): the constraint, the measurements, the decisions that were reversed, and the defects the benchmarks found in Velox.
- [Architecture](architecture.md): package layout and design rules.
- [Decision records](adr/README.md): one decision per file, with the alternatives.
