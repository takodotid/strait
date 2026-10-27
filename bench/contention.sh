#!/usr/bin/env bash
# Envoy against HAProxy when the nodes are short of CPU. Runs on the clusters that
# `STRAIT_BENCH_KEEP=1 make bench` keeps, and writes _output/bench/contention.md.
#
# Both nodes are held to the host CPUs in STRAIT_BENCH_CPUS. The client runs make bench's load
# through Envoy and through HAProxy, alternating A B B A so that neither always goes first:
# first with no neighbour, then with a neighbour pod per site that keeps two threads busy.
# The nodes get every host CPU back at the end. The clusters stay.
#
#   STRAIT_BENCH_CPUS=4-7     host CPUs for both nodes
#   STRAIT_BENCH_BLOCKS=4     A B B A blocks per condition
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
BENCH=$ROOT/bench
OUT=$ROOT/_output/bench
KCFG=$OUT/kubeconfig
CPUS=${STRAIT_BENCH_CPUS:-4-7}
BLOCKS=${STRAIT_BENCH_BLOCKS:-4}
NODES=(strait-bench-a-control-plane strait-bench-b-control-plane)
LOAD_CONNS=$(awk -F'[= ]' '/^LOAD_CONNS=/ {print $2}' "$BENCH/run.sh")
LOAD_RATE=$(awk -F'[= ]' '/^LOAD_RATE=/ {print $2}' "$BENCH/run.sh")
RAW=$OUT/contention.txt

declare -A TARGET=([envoy]=bench.bench.svc:7000 [haproxy]=haproxy-b.bench.svc:7000)

ks() { local s=$1; shift; kubectl --kubeconfig "$KCFG" --context "kind-strait-bench-$s" "$@"; }
log() { printf '%s %s\n' "$(date +%H:%M:%S)" "$*" >&2; }

# one CONDITION PROXY prints "condition proxy p50_us p99_us achieved_rps loadavg1".
one() {
  local r
  r=$(ks b -n bench exec client -- tcpbench load -addr "${TARGET[$2]}" -conns "$LOAD_CONNS" -rate "$LOAD_RATE" -duration 14s -warmup 2s)
  echo "$1 $2 $(sed -n 's/.*"achieved_rps":\([0-9.]*\).*"p50_us":\([0-9.]*\).*"p99_us":\([0-9.]*\).*/\2 \3 \1/p' <<<"$r") $(cut -d' ' -f1 /proc/loadavg)"
}

condition() { # name
  local i
  log "$1: $BLOCKS blocks"
  for i in $(seq 1 "$BLOCKS"); do
    one "$1" envoy
    one "$1" haproxy
    one "$1" haproxy
    one "$1" envoy
  done >>"$RAW"
}

# summary CONDITION PROXY COLUMN prints "median (min to max)" of a column of the raw lines.
summary() {
  awk -v c="$1" -v p="$2" -v k="$3" '$1 == c && $2 == p {print $k}' "$RAW" | sort -n |
    awk '{a[NR] = $1} END {m = NR % 2 ? a[(NR + 1) / 2] : (a[NR / 2] + a[NR / 2 + 1]) / 2; printf "%.0f (%.0f to %.0f)", m, a[1], a[NR]}'
}

restore() { docker update --cpuset-cpus "0-$(($(nproc) - 1))" "${NODES[@]}" >/dev/null; }

main() {
  for c in strait-bench-a strait-bench-b; do
    kind get clusters | grep -qx "$c" || { echo "$c is missing. Run STRAIT_BENCH_KEEP=1 make bench first." >&2; exit 1; }
  done
  # The gateway path on the direct link, as make bench leaves it.
  ks b patch sitelink a-direct --type merge -p '{"spec":{"direct":{"endpointDirect":false}}}' >/dev/null
  ks b patch sitelink a-wireguard --type merge -p '{"spec":{"cost":20}}' >/dev/null
  trap restore EXIT
  docker update --cpuset-cpus "$CPUS" "${NODES[@]}" >/dev/null
  : >"$RAW"
  condition "no-neighbour"
  for s in a b; do ks "$s" apply -f "$BENCH/manifests/neighbour.yaml" >/dev/null; done
  for s in a b; do ks "$s" -n bench rollout status deploy/neighbour --timeout 3m >/dev/null; done
  condition "neighbour"
  for s in a b; do ks "$s" delete -f "$BENCH/manifests/neighbour.yaml" --wait=false >/dev/null; done
  {
    echo "# Envoy against HAProxy under CPU contention"
    echo
    echo "Both nodes on host CPUs $CPUS. $LOAD_CONNS connections, $LOAD_RATE requests per second each. Median, then the lowest and highest run in brackets."
    echo
    echo "| Condition | Proxy | Runs | Under load p50, µs | Under load p99, µs | Load average, 1 min |"
    echo "|---|---|---|---|---|---|"
    for c in no-neighbour neighbour; do
      for p in envoy haproxy; do
        echo "| $c | $p | $(awk -v c="$c" -v p="$p" '$1 == c && $2 == p' "$RAW" | wc -l) | $(summary "$c" "$p" 3) | $(summary "$c" "$p" 4) | $(awk -v c="$c" -v p="$p" '$1 == c && $2 == p {print $6}' "$RAW" | sort -n | sed -n '1p;$p' | paste -sd' ' | sed 's/ / to /') |"
      done
    done
    echo
    echo "Runs that held less than 98% of the target rate: $(awk -v t="$((LOAD_CONNS * LOAD_RATE))" '$5 < 0.98 * t' "$RAW" | wc -l)."
  } >"$OUT/contention.md"
  log "results: $OUT/contention.md"
}

main "$@"
