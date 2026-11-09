#!/usr/bin/env bash
#
# Checks the Compose stack's two nginx configs, the front proxy (nginx.conf)
# and the dashboard's own nginx (dashboard-nginx.conf):
#   - nginx -t accepts each one;
#   - a link's token reaches neither access log, from the path, the query or
#     the Referer;
#   - the dashboard's pages, the hosted invoice page among them, carry the
#     security headers, once each.
#
# The images and the config files each container gets are read from
# docker-compose.yml and Dockerfile.dashboard, so the check runs what the
# stack runs. Three containers start on a new Docker network: the front
# proxy, the dashboard's nginx with a stub page, and a stub API. All are
# named hc-nginx-check-<pid> and removed on exit. CI runs it in the lint job.
set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
compose="$root/deploy/compose"
name="hc-nginx-check-$$"
work=$(mktemp -d)
cleanup() {
  docker rm -f "$name-proxy" "$name-dash" "$name-api" >/dev/null 2>&1 || true
  docker network rm "$name" >/dev/null 2>&1 || true
  rm -rf "$work"
}
trap cleanup EXIT

proxy_image=$(sed -n 's/^ *image: \(nginx:[^ ]*\)$/\1/p' "$compose/docker-compose.yml")
dash_image=$(sed -n 's/^FROM \(nginx:[^ ]*\)$/\1/p' "$compose/Dockerfile.dashboard")
if [ -z "$proxy_image" ] || [ -z "$dash_image" ]; then
  echo "no nginx image in docker-compose.yml or Dockerfile.dashboard" >&2
  exit 1
fi
proxy_files=() dash_files=()
while read -r src dst; do proxy_files+=(-v "$compose/$src:$dst:ro"); done < <(
  sed -n 's|^ *- \./\([^:]*\.conf\):\([^:]*\):ro$|\1 \2|p' "$compose/docker-compose.yml")
while read -r src dst; do dash_files+=(-v "$root/$src:$dst:ro"); done < <(
  sed -n 's|^COPY \(deploy/compose/[^ ]*\.conf\) \([^ ]*\)$|\1 \2|p' "$compose/Dockerfile.dashboard")

# The stub dashboard build, and a stub API that answers every request.
mkdir -p "$work/html/assets"
echo '<!doctype html><title>stub</title>' > "$work/html/index.html"
echo 'console.log("stub")' > "$work/html/assets/app.js"
echo 'server { listen 8080; access_log off; location / { return 200 "{}"; } }' > "$work/api.conf"
chmod -R a+rX "$work"

docker network create "$name" >/dev/null
echo "== nginx -t: dashboard-nginx.conf on $dash_image"
docker run --rm "${dash_files[@]}" "$dash_image" nginx -t
docker run -d --name "$name-api" --network "$name" --network-alias hardcount-api \
  -v "$work/api.conf:/etc/nginx/conf.d/default.conf:ro" "$proxy_image" >/dev/null
docker run -d --name "$name-dash" --network "$name" --network-alias hardcount-dashboard \
  "${dash_files[@]}" -v "$work/html:/usr/share/nginx/html:ro" "$dash_image" >/dev/null
echo "== nginx -t: nginx.conf on $proxy_image"
docker run --rm --network "$name" "${proxy_files[@]}" "$proxy_image" nginx -t
docker run -d --name "$name-proxy" --network "$name" -p 127.0.0.1::80 \
  "${proxy_files[@]}" "$proxy_image" >/dev/null
base="http://127.0.0.1:$(docker port "$name-proxy" 80/tcp | head -1 | sed 's/.*://')"
for _ in $(seq 50); do curl -fs -o /dev/null "$base/" && break; sleep 0.2; done

failures=0
fail() { printf 'FAIL  %s\n' "$1"; failures=$((failures + 1)); }

echo "== Security headers on the dashboard's pages"
for path in / /invoice/hc_pinv_secret00 /assets/app.js /assets/missing.js; do
  headers=$(curl -sS -o /dev/null -D - "$base$path" | tr -d '\r')
  for want in "X-Frame-Options: DENY" "Content-Security-Policy: frame-ancestors 'none'" \
      "X-Content-Type-Options: nosniff" "Referrer-Policy: strict-origin-when-cross-origin"; do
    n=$(grep -ciF -- "$want" <<<"$headers" || true)
    if [ "$n" = 1 ]; then printf 'ok    %s  %s\n' "$path" "$want"; else fail "$path  $want (seen $n times)"; fi
  done
done

echo "== Link tokens in the access logs"
# Each request is "path referer". Every token holds the word "secret".
while read -r path referer; do
  curl -sS -o /dev/null -H "Referer: $base$referer" "$base$path"
done <<'EOF'
/invoice/hc_pinv_secret01?paid=1 /invoice/hc_pinv_secret02
/v1/public/invoices/hc_pinv_secret03/pdf /invoice/hc_pinv_secret04
/v1/public/payment-updates/hc_pt_secret05/checkout /update-payment?token=secret06
/v1/public/cost-dashboard/hc_pcd_secret07 /
/v1/auth/invite/secret08 /accept-invite?token=secret09
/reset-password?token=secret10&next=1 /
/v1/auth/password-reset/check?token=secret11 /reset-password?token=secret12
/update-payment?a=1&token=secret13&token=secret14 /
/invoices/hc_inv_visible /invoices
EOF
for c in proxy dash; do
  for _ in $(seq 25); do
    docker logs "$name-$c" > "$work/$c.log" 2>&1
    grep -q hc_inv_visible "$work/$c.log" && break
    sleep 0.2
  done
  echo "-- $c access log:"
  grep '" [0-9][0-9][0-9] ' "$work/$c.log" || true
  if grep -q secret "$work/$c.log"; then fail "$c log holds a token"; else echo "ok    $c log holds no token"; fi
  grep -q 'GET /invoices/hc_inv_visible ' "$work/$c.log" || fail "$c log lacks a path with no token"
done

[ "$failures" = 0 ] || { echo "$failures failed"; exit 1; }
echo "All nginx checks passed."
