#!/bin/bash
# usage: serve.sh <worktree>  — (re)build API on :8185 and vite on :5185 from that worktree
WT=$1; D=/tmp/claude-0/ux/build-admin
for f in api vite; do [ -f $D/$f.pid ] && kill $(cat $D/$f.pid) 2>/dev/null; done
(cd $WT && go build -o $D/velox-api ./cmd/velox) || exit 1
cd $WT; PORT=8185 APP_ENV=local DATABASE_URL="postgres://velox:velox@localhost:5432/velox?sslmode=disable" $EXTRA nohup $D/velox-api > $D/api.log 2>&1 &
echo $! > $D/api.pid
cd $WT/web-v2; VELOX_API_TARGET=http://localhost:8185 nohup npx vite --host 127.0.0.1 --port 5185 --strictPort > $D/vite.log 2>&1 &
echo $! > $D/vite.pid
sleep 7; curl -s -o /dev/null -w "vite %{http_code}\n" http://127.0.0.1:5185/; curl -s -o /dev/null -w "api %{http_code}\n" http://localhost:8185/health
