# Images the CI scripts run, pinned by digest so a re-pushed tag cannot change
# what runs. Sourced, not run.
# shellcheck shell=bash disable=SC2034
POSTGRES15_IMAGE="postgres:15.19-alpine@sha256:a8e042598834300dfaa17a54f695b87b7f25d3490433bbbe2ef3bffbb9747f24"
POSTGRES16_IMAGE="postgres:16.15-alpine@sha256:721873c34ceb9f8d8fc265984940dc982404c105f19ad51be9fdc5970a6080ea"
POSTGRES18_IMAGE="postgres:18.6-alpine@sha256:898c60cb390d1085bc97bdd6a1ae968757421df394b071848b52b1c09c89d6a5"
# scripts/pgbouncer-test.sh. A third-party image that CI runs with
# --network host.
PGBOUNCER_IMAGE="edoburu/pgbouncer:v1.26.1-p0@sha256:488422e38cff912895d0d65d8963fcd306e6ac589a724398b6eb8e294e94d624"
