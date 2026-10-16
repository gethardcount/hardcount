#!/bin/bash
cd /home/user/velox/.claude/worktrees/catalog && go build -o /tmp/claude-0/ux/build-catalog/velox-api ./cmd/velox || exit 1
kill $(cat /tmp/claude-0/ux/build-catalog/api.pid) 2>/dev/null; sleep 1
cd /tmp/claude-0/ux/build-catalog && DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" APP_ENV=local PORT=8184 nohup ./velox-api > api.log 2>&1 &
echo $! > /tmp/claude-0/ux/build-catalog/api.pid
for i in $(seq 1 30); do curl -s -o /dev/null localhost:8184/health && break; sleep 0.5; done; echo api up
