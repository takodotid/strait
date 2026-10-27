#!/usr/bin/env bash
# Gateway benchmark. `make bench` runs it. Method and results: docs/benchmark.md.
#
# Creates the kind clusters strait-bench-a and strait-bench-b, installs Strait and a HAProxy pair,
# measures every path in interleaved rounds, writes _output/bench/results.md and results.json,
# then deletes the clusters.
#
#   STRAIT_BENCH_KEEP=1        keep the clusters. A later run reuses them.
#   STRAIT_BENCH_SKIP_BUILD=1  use the images of an earlier run
#   STRAIT_BENCH_ROUNDS=N      rounds, 5 by default
#   STRAIT_BENCH_PATHS="..."   paths in measuring order. pod-ip adds the client dialing the server pod's address.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BENCH=$ROOT/bench
OUT=$ROOT/_output/bench
RAW=$OUT/raw
KCFG=$OUT/kubeconfig
ROUNDS=${STRAIT_BENCH_ROUNDS:-5}
PATHS=${STRAIT_BENCH_PATHS:-strait-gateway-direct haproxy strait-endpoint-direct strait-gateway-wireguard}

# Measurements. Request and response are 64 bytes each.
RR_RATE=1000        # requests per second on one open connection
RR_SECONDS=10
CONNECT_COUNT=500   # new connections, one after another
STREAM_SECONDS=10   # after one second of warmup
LOAD_CONNS=50       # connections for the CPU measurement
LOAD_RATE=100       # requests per second on each of them
LOAD_SECONDS=24
CPU_FROM=4          # the CPU window starts this long after the load starts
CPU_SECONDS=16      # and lasts this long; the idle window too

# Fixture.
CLUSTER_A=strait-bench-a
CLUSTER_B=strait-bench-b
CTX_A=kind-$CLUSTER_A
CTX_B=kind-$CLUSTER_B
NODE_A=$CLUSTER_A-control-plane
NODE_B=$CLUSTER_B-control-plane
NET_WAN=strait-net          # each node's primary network; the wireguard link runs over it
NET_WAN_SUBNET=172.30.0.0/24
NET_DIRECT=strait-direct    # the private link between the sites
NET_DIRECT_SUBNET=172.31.0.0/24
DIRECT_IP_A=172.31.0.12
DIRECT_IP_B=172.31.0.22
POD_CIDR_A=$(awk '/podSubnet/ {print $2}' "$BENCH/kind-a.yaml")
POD_CIDR_B=$(awk '/podSubnet/ {print $2}' "$BENCH/kind-b.yaml")
STRAIT_IMAGE=strait-bench-strait:dev
TCPBENCH_IMAGE=strait-bench-tcpbench:dev
HAPROXY_IMAGE=$(awk '/image: haproxy:/ {print $2; exit}' "$BENCH/manifests/haproxy-a.yaml")
ENVOY_IMAGE=$(helm template x "$ROOT/deploy/helm/strait" --set site.name=x | awk '/image: .*envoy/ {print $2; exit}')
APP=bench.bench.svc:7000    # the SiteService's application name and port
SRV_PORT=7001               # the server's container port, the member Service's target port

log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" >&2; }
ka() { kubectl --kubeconfig "$KCFG" --context "$CTX_A" "$@"; }
kb() { kubectl --kubeconfig "$KCFG" --context "$CTX_B" "$@"; }
ks() { local s=$1; shift; kubectl --kubeconfig "$KCFG" --context "kind-strait-bench-$s" "$@"; }
client() { kb -n bench exec client -- tcpbench "$@"; }
strait() { "$TMP/strait" --kubeconfig "$KCFG" "$@"; }

# wait_for SECONDS WHAT COMMAND... runs COMMAND every second until it succeeds.
wait_for() {
  local deadline=$((SECONDS + $1)) what=$2
  shift 2
  until "$@" >/dev/null 2>&1; do
    if ((SECONDS >= deadline)); then
      log "timed out waiting for $what"
      return 1
    fi
    sleep 1
  done
}

TMP=$(mktemp -d "${TMPDIR:-/tmp}/strait-bench.XXXXXX")
cleanup() {
  rm -rf "$TMP"
  if [ "${STRAIT_BENCH_KEEP:-}" = 1 ]; then
    log "keeping $CLUSTER_A and $CLUSTER_B; kubeconfig: $KCFG"
    return
  fi
  for c in "$CLUSTER_A" "$CLUSTER_B"; do kind delete cluster --name "$c" --kubeconfig "$KCFG" || true; done
}
trap cleanup EXIT

build() {
  (cd "$ROOT" && go build -o "$TMP/tcpbench" ./bench/tcpbench && go build -o "$TMP/strait" ./cmd/strait)
  if [ "${STRAIT_BENCH_SKIP_BUILD:-}" = 1 ]; then
    return
  fi
  log "building images"
  make -C "$ROOT" images IMAGE="${STRAIT_IMAGE%:*}" TAG="${STRAIT_IMAGE##*:}"
  (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$TMP/image/tcpbench" ./bench/tcpbench)
  cp "$BENCH/Dockerfile" "$TMP/image/"
  docker build -q -t "$TCPBENCH_IMAGE" "$TMP/image" >/dev/null
  for img in "$ENVOY_IMAGE" "$HAPROXY_IMAGE"; do
    docker image inspect "$img" >/dev/null 2>&1 || docker pull -q "$img" >/dev/null
  done
}

clusters() {
  docker network inspect "$NET_WAN" >/dev/null 2>&1 || docker network create --driver bridge --subnet "$NET_WAN_SUBNET" "$NET_WAN"
  docker network inspect "$NET_DIRECT" >/dev/null 2>&1 || docker network create --driver bridge --subnet "$NET_DIRECT_SUBNET" --internal "$NET_DIRECT"
  for s in a b; do
    local c=strait-bench-$s
    if kind get clusters | grep -qx "$c"; then
      kind export kubeconfig --name "$c" --kubeconfig "$KCFG"
    else
      log "creating $c"
      KIND_EXPERIMENTAL_DOCKER_NETWORK=$NET_WAN kind create cluster --name "$c" --config "$BENCH/kind-$s.yaml" --kubeconfig "$KCFG"
    fi
  done
  attach "$NODE_A" "$DIRECT_IP_A"
  attach "$NODE_B" "$DIRECT_IP_B"
}

attach() { # node address
  docker inspect "$1" --format '{{json .NetworkSettings.Networks}}' | grep -q "\"$NET_DIRECT\"" ||
    docker network connect --ip "$2" "$NET_DIRECT" "$1"
}

# kind load docker-image fails with Docker's containerd image store, so every image goes through a one-platform archive.
load_images() {
  log "loading images"
  for img in "$STRAIT_IMAGE" "$ENVOY_IMAGE" "$HAPROXY_IMAGE" "$TCPBENCH_IMAGE"; do
    local tar=$TMP/image.tar
    docker save --platform linux/amd64 -o "$tar" "$img"
    for c in "$CLUSTER_A" "$CLUSTER_B"; do kind load image-archive --name "$c" "$tar"; done
    rm -f "$tar"
  done
}

install_strait() { # site
  local s=$1
  ks "$s" create namespace strait-system --dry-run=client -o yaml | ks "$s" apply -f - >/dev/null
  ks "$s" label namespace strait-system pod-security.kubernetes.io/enforce=privileged --overwrite >/dev/null
  if ! ks "$s" -n strait-system get secret strait-wireguard >/dev/null 2>&1; then
    # The private key lives in a file that only this user reads, and only until the Secret holds it.
    mkdir -p -m 700 "$TMP/keys"
    "$TMP/tcpbench" wgkey "$TMP/keys/$s" >/dev/null
    ks "$s" -n strait-system create secret generic strait-wireguard --from-file=privateKey="$TMP/keys/$s" >/dev/null
    rm -f "$TMP/keys/$s"
  fi
  log "installing Strait at site $s"
  helm upgrade --install strait "$ROOT/deploy/helm/strait" --kubeconfig "$KCFG" --kube-context "kind-strait-bench-$s" \
    -n strait-system -f "$BENCH/values.yaml" --set site.name="$s" --wait --timeout 5m >/dev/null
}

workloads() {
  log "deploying the server, the client and the HAProxy pair"
  for s in a b; do ks "$s" create namespace bench --dry-run=client -o yaml | ks "$s" apply -f - >/dev/null; done
  ka apply -f "$BENCH/manifests/server.yaml" >/dev/null
  ka -n bench rollout status deploy/srv --timeout 3m >/dev/null
  ka -n bench create configmap haproxy-a --from-file=haproxy.cfg="$BENCH/haproxy/a.cfg" --dry-run=client -o yaml | ka apply -f - >/dev/null
  ka apply -f "$BENCH/manifests/haproxy-a.yaml" >/dev/null
  sed "s|PEER_DIRECT_IP|$DIRECT_IP_A|" "$BENCH/haproxy/b.cfg" >"$TMP/b.cfg"
  kb -n bench create configmap haproxy-b --from-file=haproxy.cfg="$TMP/b.cfg" --dry-run=client -o yaml | kb apply -f - >/dev/null
  kb apply -f "$BENCH/manifests/client.yaml" -f "$BENCH/manifests/haproxy-b.yaml" >/dev/null
  # A changed configuration rolls HAProxy.
  ka -n bench patch deploy haproxy -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"strait-bench/config\":\"$(sha256sum <"$BENCH/haproxy/a.cfg" | cut -c1-16)\"}}}}}" >/dev/null
  kb -n bench patch deploy haproxy -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"strait-bench/config\":\"$(sha256sum <"$TMP/b.cfg" | cut -c1-16)\"}}}}}" >/dev/null
  ka -n bench rollout status deploy/haproxy --timeout 3m >/dev/null
  kb -n bench rollout status deploy/haproxy --timeout 3m >/dev/null
  kb -n bench wait --for=condition=Ready pod/client --timeout 3m >/dev/null
}

links() {
  log "linking the sites"
  wait_for 120 "site a's public key" public_key a
  wait_for 120 "site b's public key" public_key b
  local pub_a pub_b wan_a
  pub_a=$(public_key a)
  pub_b=$(public_key b)
  wan_a=$(docker inspect "$NODE_A" --format "{{(index .NetworkSettings.Networks \"$NET_WAN\").IPAddress}}")
  sed -e "s|PEER_PUBLIC_KEY|$pub_b|" -e "s|PEER_DIRECT_IP|$DIRECT_IP_B|" "$BENCH/manifests/links-a.yaml" | ka apply -f - >/dev/null
  sed -e "s|PEER_PUBLIC_KEY|$pub_a|" -e "s|PEER_DIRECT_IP|$DIRECT_IP_A|" -e "s|PEER_WAN_IP|$wan_a|" "$BENCH/manifests/links-b.yaml" | kb apply -f - >/dev/null
  for s in a b; do ks "$s" apply -f "$BENCH/manifests/siteservice.yaml" >/dev/null; done
}

public_key() { # site
  local k
  k=$(ks "$1" get site "$1" -o jsonpath='{.status.wireGuardPublicKey}')
  [ -n "$k" ] && echo "$k"
}

# Endpoint-direct needs each site's pod addresses routed over the direct link, and kept unmasqueraded.
node_routes() {
  route "$NODE_A" "$POD_CIDR_B" "$DIRECT_IP_B" "$DIRECT_IP_A"
  route "$NODE_B" "$POD_CIDR_A" "$DIRECT_IP_A" "$DIRECT_IP_B"
}

route() { # node destination via own-address
  local dev
  dev=$(docker exec "$1" ip -o -4 addr show | awk -v ip="$4" '$4 ~ "^"ip"/" {print $2}')
  docker exec "$1" ip route replace "$2" via "$3" dev "$dev"
  docker exec "$1" sh -c "iptables -t nat -C KIND-MASQ-AGENT -d $2 -m comment --comment strait-bench-nomasq -j RETURN 2>/dev/null ||
    iptables -t nat -I KIND-MASQ-AGENT 1 -d $2 -m comment --comment strait-bench-nomasq -j RETURN"
}

link_state() { ks "$1" get sitelink "$2" -o jsonpath='{.status.healthy} {.status.active}'; }
link_is() { [ "$(link_state "$1" "$2")" = "$3" ]; }

# Addresses that the checks compare against.
addresses() {
  GW_A=$(ka get site a -o jsonpath='{.status.gateway.address}')
  GW_B=$(kb get site b -o jsonpath='{.status.gateway.address}')
  IMPORT_PORT=$(kb -n bench get import bench-srv-a -o jsonpath='{.status.gatewayPort}')
  CLIENT_IP=$(kb -n bench get pod client -o jsonpath='{.status.podIP}')
  SRV_IP=$(ka -n bench get pod -l app=srv -o jsonpath='{.items[0].status.podIP}')
  HAPROXY_A=$(ka -n bench get pod -l app=haproxy -o jsonpath='{.items[0].status.podIP}')
  [ -n "$GW_A" ] && [ -n "$GW_B" ] && [ -n "$IMPORT_PORT" ] && [ -n "$CLIENT_IP" ] && [ -n "$SRV_IP" ] && [ -n "$HAPROXY_A" ]
}

routed_to_gateway() { [ "$(slice)" = "$GW_B:$IMPORT_PORT" ]; }

ready() {
  log "waiting for the links and the route"
  wait_for 180 "a's direct link" link_is a b-direct "true true"
  wait_for 180 "b's direct link" link_is b a-direct "true true"
  wait_for 180 "b's wireguard link" link_is b a-wireguard "true false"
  wait_for 120 "the import's port and the gateway addresses" addresses
  wait_for 120 "the route at b" routed_to_gateway
  strait --context "$CTX_B" -n bench status >"$OUT/strait-status-b.txt"
  strait --context "$CTX_B" links >"$OUT/strait-links-b.txt"
  strait --context "$CTX_A" links >"$OUT/strait-links-a.txt"
  strait --context "$CTX_B" -n bench explain bench >"$OUT/strait-explain-b-gateway.txt"
}

# The addresses and port of the application name's endpoints at site b.
slice() {
  kb -n bench get endpointslice -l kubernetes.io/service-name=bench \
    -o jsonpath='{range .items[*]}{range .endpoints[*]}{.addresses[0]}{" "}{end}{":"}{.ports[0].port}{end}' | sed 's/ :/:/'
}

# Proxy containers: node and cgroup directory, for the CPU readings.
declare -A CPU_NODE CPU_CG
proxies() {
  local p s ns sel ctr id
  for p in envoy-a:a:strait-system:app.kubernetes.io/component=gateway:envoy envoy-b:b:strait-system:app.kubernetes.io/component=gateway:envoy \
    haproxy-a:a:bench:app=haproxy:haproxy haproxy-b:b:bench:app=haproxy:haproxy; do
    IFS=: read -r p s ns sel ctr <<<"$p"
    id=$(ks "$s" -n "$ns" get pod -l "$sel" -o jsonpath="{.items[0].status.containerStatuses[?(@.name==\"$ctr\")].containerID}")
    id=${id#*://}
    CPU_NODE[$p]=strait-bench-$s-control-plane
    CPU_CG[$p]=$(docker exec "${CPU_NODE[$p]}" find /sys/fs/cgroup -type d -name "*$id*" -print -quit)
    [ -n "${CPU_CG[$p]}" ] || { log "no cgroup for $p"; return 1; }
  done
  # Envoy at b holds the WireGuard interface. Its /proc/PID/net/dev shows the pod's interfaces.
  id=$(kb -n strait-system get pod -l app.kubernetes.io/component=gateway -o jsonpath='{.items[0].status.containerStatuses[?(@.name=="envoy")].containerID}')
  GW_B_PID=$(docker exec "$NODE_B" crictl inspect --output go-template --template '{{.info.pid}}' "${id#*://}")
}

cpu_read() { # phase file
  local p
  for p in envoy-a envoy-b haproxy-a haproxy-b; do
    docker exec "${CPU_NODE[$p]}" sh -c 'echo "$(date +%s%N) $(sed -n "s/^usage_usec //p" "$1/cpu.stat")"' sh "${CPU_CG[$p]}" |
      sed "s/^/$1 $p /" >>"$2"
  done
  # The whole host's busy time, in the same unit, to show how busy the host was in the window.
  echo "$1 host $(date +%s%N) $(awk -v hz="$(getconf CLK_TCK)" '/^cpu / {printf "%.0f", ($2 + $3 + $4 + $7 + $8 + $9) * 1e6 / hz}' /proc/stat)" >>"$2"
}

# Bytes received on the WireGuard interface of gateway b.
wg_rx() { docker exec "$NODE_B" cat "/proc/$GW_B_PID/net/dev" | awk '$1 ~ /^st[0-9a-f]+:$/ {print $2; exit}'; }

# wg_carries reports whether a one-second transfer through the application name crosses the WireGuard interface.
wg_carries() {
  local w0 w1
  w0=$(wg_rx)
  client stream -addr "$APP" -duration 1s -warmup 0s >/dev/null
  w1=$(wg_rx)
  ((w1 - w0 > 10000000))
}

target() {
  case $1 in
  haproxy) echo "haproxy-b.bench.svc:7000" ;;
  pod-ip) echo "$SRV_IP:$SRV_PORT" ;;
  *) echo "$APP" ;;
  esac
}

# check PATH PHASE prints one "ok" or "fail" line per check that the traffic takes PATH.
check() {
  local p=$1 ph=$2 seen want link="" endpoints="" wg=""
  seen=$(client probe -addr "$(target "$p")" 2>/dev/null | sed -n 's/.*"peer":"\([^"]*\):[0-9]*".*/\1/p')
  case $p in
  strait-gateway-direct) want=$GW_A link="a-direct" endpoints="$GW_B:$IMPORT_PORT" wg=no ;;
  strait-gateway-wireguard) want=$GW_A link="a-wireguard" endpoints="$GW_B:$IMPORT_PORT" wg=yes ;;
  strait-endpoint-direct) want=$CLIENT_IP link="a-direct" endpoints="$SRV_IP:$SRV_PORT" wg=no ;;
  haproxy) want=$HAPROXY_A ;;
  pod-ip) want=$CLIENT_IP ;;
  esac
  result "$ph server-sees" "$seen" "$want"
  if [ -n "$link" ]; then
    result "$ph link-$link-healthy-active" "$(link_state b "$link")" "true true"
    result "$ph endpoints" "$(slice)" "$endpoints"
  fi
  case $wg in
  yes) if wg_carries; then result "$ph wireguard-carries" yes yes; else result "$ph wireguard-carries" no yes; fi ;;
  no) if wg_carries; then result "$ph wireguard-idle" no yes; else result "$ph wireguard-idle" yes yes; fi ;;
  esac
}

result() { # what got want
  if [ "$2" = "$3" ]; then echo "ok $1 $2"; else echo "fail $1 got '$2' want '$3'"; fi
}

checks_pass() { ! check "$1" now | grep -q '^fail'; }

# set_state PATH sets b's links for PATH and waits until the traffic takes it.
set_state() {
  local ed=false cost=20
  case $1 in
  strait-endpoint-direct) ed=true ;;
  strait-gateway-wireguard) cost=5 ;;
  strait-gateway-direct) ;;
  *) return 0 ;;
  esac
  kb patch sitelink a-direct --type merge -p "{\"spec\":{\"direct\":{\"endpointDirect\":$ed}}}" >/dev/null
  kb patch sitelink a-wireguard --type merge -p "{\"spec\":{\"cost\":$cost}}" >/dev/null
  wait_for 90 "the $1 state" checks_pass "$1"
}

record_env() { # file
  local b0 i0 s0 b1 i1 s1 n
  n=$(nproc)
  read -r b0 i0 s0 < <(awk '/^cpu / {print $2+$3+$4+$7+$8+$9, $5+$6, $9}' /proc/stat)
  sleep 2
  read -r b1 i1 s1 < <(awk '/^cpu / {print $2+$3+$4+$7+$8+$9, $5+$6, $9}' /proc/stat)
  {
    echo "nproc $n"
    echo "loadavg1 $(cut -d' ' -f1 /proc/loadavg)"
    awk -v b="$((b1 - b0))" -v i="$((i1 - i0))" -v s="$((s1 - s0))" -v n="$n" \
      'BEGIN {printf "host_busy_cores %.2f\nsteal_cores %.2f\n", b / (b + i) * n, s / (b + i) * n}'
    # Containers of other projects count in the total only. Their names stay out of the results.
    docker stats --no-stream --format '{{.Name}} {{.CPUPerc}}' |
      awk '{gsub("%", "", $2); if ($1 ~ /^strait-bench-/) b += $2; else o += $2} END {printf "bench_cores %.2f\nother_cores %.2f\n", b / 100, o / 100}'
  } >"$1"
}

write_meta() {
  local args
  args=$(ka -n strait-system get deploy strait-gateway -o jsonpath='{.spec.template.spec.containers[?(@.name=="envoy")].args}')
  {
    echo "paths $PATHS"
    echo "rounds $ROUNDS"
    echo "strait_commit $(git -C "$ROOT" rev-parse --short HEAD)$(git -C "$ROOT" diff --quiet HEAD || echo -dirty)"
    echo "kubernetes $(ka get node "$NODE_A" -o jsonpath='{.status.nodeInfo.kubeletVersion}')"
    echo "kube_proxy_mode $(ka -n kube-system get cm kube-proxy -o jsonpath='{.data.config\.conf}' | awk '/^mode:/ {print $2}')"
    echo "kind $(kind version | awk '{print $2}')"
    echo "docker $(docker version --format '{{.Server.Version}}')"
    echo "linux $(uname -r | cut -d- -f1)"
    echo "host_vcpus $(nproc)"
    echo "envoy $ENVOY_IMAGE"
    echo "envoy_concurrency $(echo "$args" | tr -d '[]"' | tr , '\n' | awk 'p {print; exit} $0 == "--concurrency" {p = 1}')"
    echo "haproxy $HAPROXY_IMAGE"
    echo "haproxy_nbthread $(awk '/nbthread/ {print $2}' "$BENCH/haproxy/a.cfg")"
    echo "message_bytes 64"
    echo "rr_rate $RR_RATE"
    echo "rr_seconds $RR_SECONDS"
    echo "connect_count $CONNECT_COUNT"
    echo "stream_seconds $STREAM_SECONDS"
    echo "load_conns $LOAD_CONNS"
    echo "load_rate $LOAD_RATE"
    echo "cpu_window_seconds $CPU_SECONDS"
  } >"$RAW/meta.txt"
}

# run DIR NAME ARGS... runs one client measurement into DIR/NAME.json, and records a failure as a failed check.
run() {
  local d=$1 n=$2
  shift 2
  client "$@" >"$d/$n.json" 2>"$d/$n.err" || echo "fail measure $n: $(tr '\n' ' ' <"$d/$n.err")" >>"$d/check.txt"
}

measure() { # round path
  local d=$RAW/round-$1/$2 addr w0 w1 streamed pid
  addr=$(target "$2")
  mkdir -p "$d"
  log "round $1: $2"
  if ! set_state "$2"; then
    check "$2" before >"$d/check.txt"
    return 0
  fi
  check "$2" before >"$d/check.txt"
  if [ "$1" = 1 ] && [ "$2" = strait-endpoint-direct ]; then
    strait --context "$CTX_B" -n bench explain bench >"$OUT/strait-explain-b-endpoint-direct.txt"
    kb -n bench get endpointslice -l kubernetes.io/service-name=bench -o yaml >"$OUT/endpointslice-b-endpoint-direct.yaml"
  fi
  w0=$(wg_rx)
  run "$d" rr rr -addr "$addr" -rate "$RR_RATE" -duration "${RR_SECONDS}s"
  run "$d" connect connect -addr "$addr" -count "$CONNECT_COUNT"
  run "$d" stream1 stream -addr "$addr" -streams 1 -duration "${STREAM_SECONDS}s"
  run "$d" stream4 stream -addr "$addr" -streams 4 -duration "${STREAM_SECONDS}s"
  record_env "$d/env.txt"
  run "$d" load load -addr "$addr" -conns "$LOAD_CONNS" -rate "$LOAD_RATE" -duration "${LOAD_SECONDS}s" &
  pid=$!
  sleep "$CPU_FROM"
  cpu_read start "$d/cpu.txt"
  sleep "$CPU_SECONDS"
  cpu_read end "$d/cpu.txt"
  wait "$pid"
  w1=$(wg_rx)
  check "$2" after >>"$d/check.txt"
  # Every byte of the transfers crosses the WireGuard interface on that path, and almost none on the others.
  streamed=$(cat "$d/stream1.json" "$d/stream4.json" | sed -n 's/.*"bytes":\([0-9]*\).*/\1/p' | awk '{s += $1} END {print s + 0}')
  if [ "$2" = strait-gateway-wireguard ]; then
    result "wireguard-bytes" "$((w1 - w0 >= streamed))" 1 >>"$d/check.txt"
  else
    result "wireguard-bytes" "$((w1 - w0 < 50000000))" 1 >>"$d/check.txt"
  fi
  # Latency under load compares only when the client held the rate.
  result "load-rate-held" "$(sed -n 's/.*"achieved_rps":\([0-9.]*\).*/\1/p' "$d/load.json" |
    awk -v t="$((LOAD_CONNS * LOAD_RATE))" '{print ($1 >= 0.98 * t) ? "yes" : "no, " $1}')" yes >>"$d/check.txt"
}

main() {
  rm -rf "$RAW"
  mkdir -p "$RAW" "$TMP/image"
  build
  clusters
  load_images
  install_strait a
  install_strait b
  workloads
  links
  node_routes
  ready
  proxies
  write_meta
  local r p first=${PATHS%% *}
  for r in $(seq 1 "$ROUNDS"); do
    mkdir -p "$RAW/round-$r"
    set_state "$first" || true
    log "round $r: host load and idle CPU"
    record_env "$RAW/round-$r/env.txt"
    cpu_read start "$RAW/round-$r/idle.txt"
    sleep "$CPU_SECONDS"
    cpu_read end "$RAW/round-$r/idle.txt"
    for p in $PATHS; do measure "$r" "$p"; done
  done
  set_state strait-gateway-direct || true
  "$TMP/tcpbench" report -in "$RAW" -md "$OUT/results.md" -json "$OUT/results.json"
  log "results: $OUT/results.md"
}

main "$@"
