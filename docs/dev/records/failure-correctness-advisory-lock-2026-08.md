# Correctness under failure: the retired advisory lock (2026-08)

> **Record, kept as written.** This was the appendix of
> [Correctness under failure](../../benchmarks/failure-correctness.md), moved
> here on 2026-10-22. It measured the session advisory lock that
> [ADR-114](../../adr/114-leader-leases-tick-scoped-fencing.md) replaced.

## Appendix: retired mechanism

This appendix describes the session advisory lock that ADR-114 superseded on
2026-08-30. It is kept as measured, for the record.

Singleton work (jobs only one replica may run at a time) is gated by a
Postgres advisory lock: an application-defined lock the server holds for the
session. Two failure modes are measured separately, because they behave
nothing alike:

| Failure | Time until another replica can take over |
|---|---:|
| Process dies, host survives (panic, `SIGKILL`, OOM) | **under 1 ms** |
| Host disappears without closing the socket (partition, power loss, VM terminate) | **95 s, measured** |

The first figure is a bound, not a stopwatch reading. The test starts timing
after `wait()` reports the process reaped, and the *first* lock attempt already
succeeds. So 1 ms is the cost of one round-trip check, and the lock was free
before we could look. It was identical across 6 consecutive runs, which is what
a measurement pinned to its own granularity looks like.

### The partition case, actually severed

The 90 s figure used to be arithmetic. We set the TCP keepalives (the kernel's
periodic probes that detect a silently dead peer) and computed `60 + 10×3`. It
was the only number in this document that had never been observed. It was also
the one most likely to be wrong, because keepalives depend on the network path
honouring them.

So we severed a real link. A holder process in its own container takes the lock
against Postgres over a private Docker network. Then `docker network disconnect`
removes its interface, which stops packets **without sending a FIN**. That is
what a partition, a power cut, or a security-group change looks like from the
server's side. The holder process stays alive throughout, so nothing is being
measured except Postgres deciding the peer is gone.

| Keepalives (idle / interval / count) | Predicted window | Observed |
|---|---:|---:|
| 10 / 5 / 2 | 10–20 s | **13 s**, **21 s** — released |
| **60 / 10 / 3 (production)** | **60–90 s** | **95 s** — released |
| 7200 / 75 / 9 (Postgres default) | 7200–7875 s | **still held at 248 s**, when we stopped waiting |

Predicted is a *window*, not a point. The first probe fires up to `idle`
seconds after the last activity, so where the sever lands inside that interval
moves the result. Two runs at 10/5/2 gave 13 s and 21 s. That spread is the
granularity of the measurement, not an inconsistency.

The production setting recovers in 95 s against a 60–90 s predicted window.
That is just outside it, by about the 2 s polling interval plus scheduling
delay. The default is what this code did before the keepalive fix. It was still
holding the lock 2.6× longer than that when the experiment ended, and by
arithmetic would hold it for over two hours.

**One trap, because it cost us a wrong answer first.** The holder session must
be **idle**, not running a query. Our first attempt held the lock with
`pg_sleep(3600)`. The lock survived 185 s of partition with aggressive 10/5/2
keepalives, which looked like the mechanism failing. It was not. A backend
executing a query never touches its socket, so it cannot notice the peer is
gone, whatever the kernel concludes. `pg_stat_activity` said `state=active`,
and that was the sign.

The real scheduler holds this lock on an idle connection while work happens
elsewhere, so the test must too. Reproduce with a session that is `state=idle`,
or you will measure the wrong thing. The packaged drill asserts this and
refuses to run otherwise:

```bash
./scripts/partition-drill.sh 60 10 3   # production setting
./scripts/partition-drill.sh           # no SET: the pre-fix default
```

Before this work, the second number (takeover after a silent host loss) was
**7,875 s (2 h 11 m)**: the Postgres default keepalive train. During that
window every replica skips its tick, and billing stops with no error and no log
line at `Info`. See `docs/ops/postgres-requirements.md`.
