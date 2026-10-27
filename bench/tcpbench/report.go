package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// The raw layout written by bench/run.sh:
//
//	meta.txt                     "key value" lines: versions and parameters
//	round-N/env.txt              "key value" lines: host load before the round
//	round-N/idle.txt             CPU readings with no traffic: "phase proxy t_ns usage_usec"
//	round-N/PATH/{rr,connect,stream1,stream4,load}.json
//	round-N/PATH/cpu.txt         CPU readings around the load window
//	round-N/PATH/check.txt       "ok|fail what" lines from the path checks

// pathLabels names each path in the tables, in the order they are measured.
var pathLabels = map[string]string{
	"strait-gateway-direct":    "Strait gateway (Envoy), direct link",
	"haproxy":                  "HAProxy pair, direct link",
	"strait-endpoint-direct":   "Strait endpoint-direct, no proxy",
	"strait-gateway-wireguard": "Strait gateway (Envoy), WireGuard link",
	"pod-ip":                   "Pod IP, no Service, no proxy",
}

// baseline is the path with no proxy. The other paths are compared with it round by round.
const baseline = "strait-endpoint-direct"

// pathProxies lists the proxy containers on each path, at site a then site b.
var pathProxies = map[string][2]string{
	"strait-gateway-direct":    {"envoy-a", "envoy-b"},
	"strait-gateway-wireguard": {"envoy-a", "envoy-b"},
	"haproxy":                  {"haproxy-a", "haproxy-b"},
}

// Stat is one metric of one path across rounds.
type Stat struct {
	Values []float64 `json:"values"`
	Median float64   `json:"median"`
	Min    float64   `json:"min"`
	Max    float64   `json:"max"`
}

func stat(v []float64) Stat {
	s := Stat{Values: v}
	if len(v) == 0 {
		return Stat{Median: math.NaN(), Min: math.NaN(), Max: math.NaN()}
	}
	c := slices.Clone(v)
	slices.Sort(c)
	s.Min, s.Max = c[0], c[len(c)-1]
	if n := len(c); n%2 == 1 {
		s.Median = c[n/2]
	} else {
		s.Median = (c[n/2-1] + c[n/2]) / 2
	}
	return s
}

type cpuReading struct {
	tNS   float64
	usage float64 // microseconds
}

// readCPU parses "phase proxy t_ns usage_usec" lines into readings per phase and proxy.
func readCPU(file string) (map[string]map[string]cpuReading, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]map[string]cpuReading{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) != 4 {
			continue
		}
		t, err1 := strconv.ParseFloat(fs[2], 64)
		u, err2 := strconv.ParseFloat(fs[3], 64)
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("%s: bad line %q", file, sc.Text())
		}
		if out[fs[0]] == nil {
			out[fs[0]] = map[string]cpuReading{}
		}
		out[fs[0]][fs[1]] = cpuReading{t, u}
	}
	return out, sc.Err()
}

// cores returns each proxy's CPU use between the start and end phases, in cores.
func cores(r map[string]map[string]cpuReading) map[string]float64 {
	out := map[string]float64{}
	for p, s := range r["start"] {
		e, ok := r["end"][p]
		if !ok || e.tNS <= s.tNS {
			continue
		}
		out[p] = (e.usage - s.usage) / 1e6 / ((e.tNS - s.tNS) / 1e9)
	}
	return out
}

func readKV(file string) (map[string]string, []string, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	kv := map[string]string{}
	var keys []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(strings.TrimSpace(sc.Text()), " ")
		if k == "" {
			continue
		}
		if _, ok := kv[k]; !ok {
			keys = append(keys, k)
		}
		kv[k] = strings.TrimSpace(v)
	}
	return kv, keys, sc.Err()
}

func readJSON(file string) (map[string]any, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	return m, nil
}

// checksPass reports whether every line of a check file starts with ok.
func checksPass(file string) (bool, []string) {
	b, err := os.ReadFile(file)
	if err != nil {
		return false, []string{err.Error()}
	}
	var bad []string
	lines := 0
	for l := range strings.Lines(string(b)) {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		lines++
		if !strings.HasPrefix(l, "ok ") {
			bad = append(bad, l)
		}
	}
	if lines == 0 {
		bad = append(bad, "no checks recorded")
	}
	return len(bad) == 0, bad
}

// Metric names, in table order.
const (
	mRRp50, mRRp90, mRRp99         = "rr_p50_us", "rr_p90_us", "rr_p99_us"
	mSetupP50, mSetupP90, mSetup99 = "setup_p50_us", "setup_p90_us", "setup_p99_us"
	mStream1, mStream4             = "stream1_gbps", "stream4_gbps"
	mLoadP50, mLoadP99, mLoadRPS   = "load_p50_us", "load_p99_us", "load_rps"
	mCPUa, mCPUb, mCPUsum          = "cpu_a_mcores_per_conn", "cpu_b_mcores_per_conn", "cpu_total_mcores_per_conn"
	mAddRR50, mAddRR99, mAddSetup  = "added_rr_p50_us", "added_rr_p99_us", "added_setup_p50_us"
	mHostBusy, mHostOther          = "host_busy_cores", "host_other_cores"
)

type pathResult struct {
	Path    string          `json:"path"`
	Label   string          `json:"label"`
	Rounds  int             `json:"rounds"`
	Invalid []string        `json:"invalid,omitempty"`
	Metrics map[string]Stat `json:"metrics"`
}

type roundEnv struct {
	Round int                `json:"round"`
	Env   map[string]string  `json:"env"`
	Idle  map[string]float64 `json:"idle_cores"`
	// Host per path: busy cores in the load window, and other containers' cores just before it.
	Host map[string][2]float64 `json:"host_busy_and_other_cores"`
}

func report(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "_output/bench/raw", "raw results directory")
	md := fs.String("md", "_output/bench/results.md", "Markdown output")
	js := fs.String("json", "_output/bench/results.json", "JSON output")
	_ = fs.Parse(args)

	meta, metaKeys, err := readKV(filepath.Join(*in, "meta.txt"))
	if err != nil {
		return err
	}
	paths := strings.Fields(meta["paths"])
	conns, _ := strconv.ParseFloat(meta["load_conns"], 64)
	if len(paths) == 0 || conns <= 0 {
		return errors.New("meta.txt must set paths and load_conns")
	}
	rounds, _ := filepath.Glob(filepath.Join(*in, "round-*"))
	slices.SortFunc(rounds, func(a, b string) int { return roundNum(a) - roundNum(b) })

	vals := map[string]map[string][]float64{} // path -> metric -> per-round values
	pairs := map[string][]float64{}           // metric -> per-round Envoy minus HAProxy
	invalid := map[string][]string{}
	var envs []roundEnv
	for _, rd := range rounds {
		n := roundNum(rd)
		env, _, _ := readKV(filepath.Join(rd, "env.txt"))
		idleR, err := readCPU(filepath.Join(rd, "idle.txt"))
		if err != nil {
			return err
		}
		idle := cores(idleR)
		envs = append(envs, roundEnv{Round: n, Env: env, Idle: idle, Host: map[string][2]float64{}})
		round := map[string]map[string]float64{}
		for _, p := range paths {
			dir := filepath.Join(rd, p)
			if ok, bad := checksPass(filepath.Join(dir, "check.txt")); !ok {
				invalid[p] = append(invalid[p], fmt.Sprintf("round %d: %s", n, strings.Join(bad, "; ")))
				continue
			}
			m := map[string]float64{}
			get := func(file, key, metric string) error {
				j, err := readJSON(filepath.Join(dir, file))
				if err != nil {
					return err
				}
				v, ok := j[key].(float64)
				if !ok {
					return fmt.Errorf("%s/%s: no %s", dir, file, key)
				}
				m[metric] = v
				return nil
			}
			for _, g := range []struct{ file, key, metric string }{
				{"rr.json", "p50_us", mRRp50}, {"rr.json", "p90_us", mRRp90}, {"rr.json", "p99_us", mRRp99},
				{"connect.json", "p50_us", mSetupP50}, {"connect.json", "p90_us", mSetupP90}, {"connect.json", "p99_us", mSetup99},
				{"stream1.json", "gbps", mStream1}, {"stream4.json", "gbps", mStream4},
				{"load.json", "p50_us", mLoadP50}, {"load.json", "p99_us", mLoadP99}, {"load.json", "achieved_rps", mLoadRPS},
			} {
				if err := get(g.file, g.key, g.metric); err != nil {
					return err
				}
			}
			r, err := readCPU(filepath.Join(dir, "cpu.txt"))
			if err != nil {
				return err
			}
			load := cores(r)
			m[mHostBusy] = load["host"]
			if e, _, err := readKV(filepath.Join(dir, "env.txt")); err == nil {
				m[mHostOther], _ = strconv.ParseFloat(e["other_cores"], 64)
			}
			m[mCPUa], m[mCPUb] = 0, 0
			if px, ok := pathProxies[p]; ok {
				for i, name := range px {
					l, ok1 := load[name]
					id, ok2 := idle[name]
					if !ok1 || !ok2 {
						return fmt.Errorf("%s: no CPU reading for %s", dir, name)
					}
					v := (l - id) * 1000 / conns
					if i == 0 {
						m[mCPUa] = v
					} else {
						m[mCPUb] = v
					}
				}
			}
			m[mCPUsum] = m[mCPUa] + m[mCPUb]
			round[p] = m
			envs[len(envs)-1].Host[p] = [2]float64{m[mHostBusy], m[mHostOther]}
		}
		for p, m := range round {
			if vals[p] == nil {
				vals[p] = map[string][]float64{}
			}
			for k, v := range m {
				vals[p][k] = append(vals[p][k], v)
			}
			// Paired with the baseline of the same round, so drift between rounds cancels.
			if b, ok := round[baseline]; ok {
				vals[p][mAddRR50] = append(vals[p][mAddRR50], m[mRRp50]-b[mRRp50])
				vals[p][mAddRR99] = append(vals[p][mAddRR99], m[mRRp99]-b[mRRp99])
				vals[p][mAddSetup] = append(vals[p][mAddSetup], m[mSetupP50]-b[mSetupP50])
			}
		}
		e, h := round[pairEnvoy], round[pairHAProxy]
		if e != nil && h != nil {
			for _, pm := range pairMetrics {
				pairs[pm.metric] = append(pairs[pm.metric], e[pm.metric]-h[pm.metric])
			}
		}
	}
	pairStats := map[string]Stat{}
	for k, v := range pairs {
		pairStats[k] = stat(v)
	}

	var results []pathResult
	for _, p := range paths {
		r := pathResult{Path: p, Label: label(p), Invalid: invalid[p], Metrics: map[string]Stat{}}
		for k, v := range vals[p] {
			r.Metrics[k] = stat(v)
		}
		r.Rounds = len(vals[p][mRRp50])
		results = append(results, r)
	}

	out := struct {
		Meta              map[string]string `json:"meta"`
		Rounds            []roundEnv        `json:"rounds"`
		Results           []pathResult      `json:"results"`
		EnvoyMinusHAProxy map[string]Stat   `json:"envoy_minus_haproxy"`
	}{meta, envs, results, pairStats}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(*js, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.WriteFile(*md, []byte(markdown(meta, metaKeys, envs, results, pairStats)), 0o644)
}

// The pair compared round by round: Strait's Envoy gateway and the HAProxy pair, measured one after the other.
const pairEnvoy, pairHAProxy = "strait-gateway-direct", "haproxy"

var pairMetrics = []struct{ metric, name string }{
	{mRRp50, "Request/response p50, µs"},
	{mRRp99, "Request/response p99, µs"},
	{mSetupP50, "Connection setup p50, µs"},
	{mSetup99, "Connection setup p99, µs"},
	{mLoadP50, "Under load p50, µs"},
	{mLoadP99, "Under load p99, µs"},
	{mStream1, "1 stream, Gbit/s"},
	{mStream4, "4 streams, Gbit/s"},
	{mCPUsum, "CPU, both sites, millicores per connection"},
}

func roundNum(dir string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(dir), "round-"))
	return n
}

func label(p string) string {
	if l, ok := pathLabels[p]; ok {
		return l
	}
	return p
}

// cell formats a stat as "median (min-max)".
func cell(s Stat, prec int) string {
	if len(s.Values) == 0 {
		return "n/a"
	}
	f := func(x float64) string { return strconv.FormatFloat(x, 'f', prec, 64) }
	if len(s.Values) == 1 {
		return f(s.Median)
	}
	return fmt.Sprintf("%s (%s to %s)", f(s.Median), f(s.Min), f(s.Max))
}

func markdown(meta map[string]string, metaKeys []string, envs []roundEnv, results []pathResult, pairs map[string]Stat) string {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	row := func(cells ...string) { w("| %s |\n", strings.Join(cells, " | ")) }
	sep := func(n int) { row(slices.Repeat([]string{"---"}, n)...) }

	w("# Gateway benchmark results\n\n")
	w("Each cell: median over rounds, then the lowest and highest round in brackets.\n\n")

	w("## Latency, µs\n\n")
	row("Path", "Request/response p50", "p90", "p99", "Connection setup p50", "p90", "p99")
	sep(7)
	for _, r := range results {
		m := r.Metrics
		row(r.Label, cell(m[mRRp50], 0), cell(m[mRRp90], 0), cell(m[mRRp99], 0), cell(m[mSetupP50], 0), cell(m[mSetupP90], 0), cell(m[mSetup99], 0))
	}

	w("\n## Added over endpoint-direct, µs\n\n")
	w("Each round's value minus endpoint-direct's in the same round.\n\n")
	row("Path", "Request/response p50", "Request/response p99", "Connection setup p50")
	sep(4)
	for _, r := range results {
		if r.Path == baseline {
			continue
		}
		m := r.Metrics
		row(r.Label, cell(m[mAddRR50], 0), cell(m[mAddRR99], 0), cell(m[mAddSetup], 0))
	}

	w("\n## Throughput and CPU\n\n")
	w("CPU: the proxy container's CPU while %s connections each send %s requests per second, minus its idle CPU, divided by the connections. Millicores per connection.\n\n", meta["load_conns"], meta["load_rate"])
	row("Path", "1 stream, Gbit/s", "4 streams, Gbit/s", "CPU at a", "CPU at b", "CPU, both", "Under load p50, µs", "Under load p99, µs")
	sep(8)
	for _, r := range results {
		m := r.Metrics
		row(r.Label, cell(m[mStream1], 2), cell(m[mStream4], 2), cell(m[mCPUa], 1), cell(m[mCPUb], 1), cell(m[mCPUsum], 1), cell(m[mLoadP50], 0), cell(m[mLoadP99], 0))
	}

	if len(pairs) > 0 {
		w("\n## Envoy minus HAProxy\n\n")
		w("%s minus %s, round by round. The two run one after the other in each round. Positive latency and CPU favour HAProxy; positive throughput favours Envoy.\n\n", label(pairEnvoy), label(pairHAProxy))
		row("Metric", "Envoy minus HAProxy")
		sep(2)
		for _, pm := range pairMetrics {
			prec := 0
			if pm.metric == mStream1 || pm.metric == mStream4 {
				prec = 2
			} else if pm.metric == mCPUsum {
				prec = 1
			}
			row(pm.name, cell(pairs[pm.metric], prec))
		}
	}

	var bad []string
	for _, r := range results {
		for _, i := range r.Invalid {
			bad = append(bad, fmt.Sprintf("- %s, %s", r.Label, i))
		}
	}
	if len(bad) > 0 {
		w("\n## Dropped samples\n\nA path check failed, so these rounds are left out.\n\n%s\n", strings.Join(bad, "\n"))
	}

	w("\n## Host before each round\n\n")
	row("Round", "Load average, 1 min", "Host CPU busy, cores", "Benchmark containers, cores", "Other containers, cores", "Idle Envoy a+b, millicores", "Idle HAProxy a+b, millicores")
	sep(7)
	for _, e := range envs {
		row(strconv.Itoa(e.Round), e.Env["loadavg1"], e.Env["host_busy_cores"], e.Env["bench_cores"], e.Env["other_cores"],
			strconv.FormatFloat((e.Idle["envoy-a"]+e.Idle["envoy-b"])*1000, 'f', 1, 64),
			strconv.FormatFloat((e.Idle["haproxy-a"]+e.Idle["haproxy-b"])*1000, 'f', 1, 64))
	}

	w("\n## Host in each load window\n\n")
	w("Cores busy on the whole host during the load window, then the other containers' cores just before it, in brackets.\n\n")
	head := []string{"Round"}
	for _, r := range results {
		head = append(head, r.Label)
	}
	row(head...)
	sep(len(head))
	for _, e := range envs {
		cells := []string{strconv.Itoa(e.Round)}
		for _, r := range results {
			h, ok := e.Host[r.Path]
			if !ok {
				cells = append(cells, "n/a")
				continue
			}
			cells = append(cells, fmt.Sprintf("%.1f (%.1f)", h[0], h[1]))
		}
		row(cells...)
	}

	w("\n## Run\n\n")
	row("Key", "Value")
	sep(2)
	for _, k := range metaKeys {
		row(k, meta[k])
	}
	return b.String()
}
