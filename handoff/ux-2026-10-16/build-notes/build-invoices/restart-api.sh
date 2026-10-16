#!/usr/bin/env bash
cd /home/user/velox/.claude/worktrees/inv-draft-seller && go build -o /tmp/claude-0/inv-api ./cmd/velox || exit 1
pkill -f "^/tmp/claude-0/inv-api" ; sleep 1
cd /home/user/velox && PORT=8183 DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" APP_ENV=local nohup /tmp/claude-0/inv-api > /tmp/claude-0/ux/build-invoices/api.log 2>&1 &
echo $! > /tmp/claude-0/ux/build-invoices/api.pid
for i in $(seq 1 30); do curl -sf http://localhost:8183/health >/dev/null && echo up && exit 0; sleep 1; done; echo fail
