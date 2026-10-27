// Command tcpbench measures TCP latency, connection setup and throughput for the gateway benchmark.
// See docs/benchmark.md for the method and bench/run.sh for how it is driven.
//
// The client's first byte on a connection picks what the server does:
//
//	E  echo every byte back
//	D  send data until the client closes
//	I  write one JSON line with the address the server sees and its host name, then close
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmds := map[string]func([]string) error{
		"server":  server,
		"probe":   probe,
		"rr":      rr,
		"connect": connect,
		"stream":  stream,
		"load":    load,
		"report":  report,
		"wgkey":   wgkey,
	}
	f, ok := cmds[os.Args[1]]
	if !ok {
		usage()
	}
	if err := f(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: tcpbench COMMAND [flags]

In the pods:
  server   serve E, D and I connections
  probe    print the address the server sees
  rr       request/response latency on one open connection, at a fixed rate
  connect  time from dial to the first echoed response, on new connections one after another
  stream   bulk transfer from the server, over one or more connections
  load     many connections, each doing request/response at a fixed rate

On the host:
  report   turn the raw results of bench/run.sh into results.md and results.json
  wgkey    write a WireGuard private key to a file and print its public key`)
	os.Exit(2)
}

func server(args []string) error {
	fs := flag.NewFlagSet("server", flag.ExitOnError)
	listen := fs.String("listen", ":7001", "listen address")
	_ = fs.Parse(args)
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	for {
		c, err := ln.Accept()
		if err != nil {
			return err
		}
		go serve(c, host)
	}
}

func serve(c net.Conn, host string) {
	defer c.Close()
	mode := make([]byte, 1)
	if _, err := io.ReadFull(c, mode); err != nil {
		return
	}
	switch mode[0] {
	case 'E':
		buf := make([]byte, 32<<10)
		_, _ = io.CopyBuffer(c, c, buf)
	case 'D':
		buf := make([]byte, 128<<10)
		for {
			if _, err := c.Write(buf); err != nil {
				return
			}
		}
	case 'I':
		_ = json.NewEncoder(c).Encode(map[string]string{"peer": c.RemoteAddr().String(), "host": host})
	}
}

func probe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ExitOnError)
	addr := fs.String("addr", "", "server address")
	_ = fs.Parse(args)
	if err := resolve(addr); err != nil {
		return err
	}
	c, err := net.DialTimeout("tcp", *addr, 3*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte{'I'}); err != nil {
		return err
	}
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		return err
	}
	fmt.Print(line)
	return nil
}

// Latency is a summary of round-trip times, in microseconds.
type Latency struct {
	N      int     `json:"n"`
	Errors int     `json:"errors"`
	Min    float64 `json:"min_us"`
	P50    float64 `json:"p50_us"`
	P90    float64 `json:"p90_us"`
	P99    float64 `json:"p99_us"`
	P999   float64 `json:"p999_us"`
	Max    float64 `json:"max_us"`
	Mean   float64 `json:"mean_us"`
}

func summarize(d []time.Duration, errs int) Latency {
	l := Latency{N: len(d), Errors: errs}
	if len(d) == 0 {
		return l
	}
	slices.Sort(d)
	us := func(x time.Duration) float64 { return float64(x.Nanoseconds()) / 1e3 }
	q := func(p float64) float64 { return us(d[max(0, int(math.Ceil(p*float64(len(d))))-1)]) }
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	l.Min, l.P50, l.P90, l.P99, l.P999, l.Max = us(d[0]), q(0.50), q(0.90), q(0.99), q(0.999), us(d[len(d)-1])
	l.Mean = us(sum) / float64(len(d))
	return l
}

// resolve replaces the host in addr with its address, once, so no measurement includes a DNS lookup.
func resolve(addr *string) error {
	host, port, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ips, err := net.LookupHost(host)
	if err != nil {
		return err
	}
	*addr = net.JoinHostPort(ips[0], port)
	return nil
}

func emit(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }

// echoConn is one open echo connection.
type echoConn struct {
	c        net.Conn
	req, res []byte
}

func dialEcho(addr string, size int) (*echoConn, error) {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := c.Write([]byte{'E'}); err != nil {
		c.Close()
		return nil, err
	}
	e := &echoConn{c: c, req: make([]byte, size), res: make([]byte, size)}
	for i := range e.req {
		e.req[i] = byte('a' + i%26)
	}
	return e, nil
}

func (e *echoConn) roundTrip() (time.Duration, error) {
	_ = e.c.SetDeadline(time.Now().Add(3 * time.Second))
	t := time.Now()
	if _, err := e.c.Write(e.req); err != nil {
		return 0, err
	}
	if _, err := io.ReadFull(e.c, e.res); err != nil {
		return 0, err
	}
	return time.Since(t), nil
}

// paced runs fn at a fixed rate from start until end, and records the round trips after from.
// When a call is late, the next one starts at once; latency is the call's own time.
func paced(start, from, end time.Time, rate float64, fn func() (time.Duration, error)) ([]time.Duration, int, error) {
	period := time.Duration(float64(time.Second) / rate)
	var out []time.Duration
	errs := 0
	for i := 0; ; i++ {
		at := start.Add(time.Duration(i) * period)
		if at.After(end) {
			return out, errs, nil
		}
		if w := time.Until(at); w > 0 {
			time.Sleep(w)
		}
		d, err := fn()
		if err != nil {
			errs++
			return out, errs, err
		}
		if at.After(from) {
			out = append(out, d)
		}
	}
}

func rr(args []string) error {
	fs := flag.NewFlagSet("rr", flag.ExitOnError)
	addr := fs.String("addr", "", "server address")
	rate := fs.Float64("rate", 1000, "requests per second")
	duration := fs.Duration("duration", 10*time.Second, "measured time")
	warmup := fs.Duration("warmup", time.Second, "time before measuring")
	size := fs.Int("size", 64, "request and response size in bytes")
	_ = fs.Parse(args)
	if err := resolve(addr); err != nil {
		return err
	}
	e, err := dialEcho(*addr, *size)
	if err != nil {
		return err
	}
	defer e.c.Close()
	start := time.Now()
	d, errs, err := paced(start, start.Add(*warmup), start.Add(*warmup+*duration), *rate, e.roundTrip)
	if err != nil {
		return err
	}
	return emit(struct {
		Kind string  `json:"kind"`
		Rate float64 `json:"rate"`
		Size int     `json:"size"`
		Latency
	}{"rr", *rate, *size, summarize(d, errs)})
}

func connect(args []string) error {
	fs := flag.NewFlagSet("connect", flag.ExitOnError)
	addr := fs.String("addr", "", "server address")
	count := fs.Int("count", 500, "connections")
	warmup := fs.Int("warmup", 20, "connections before measuring")
	size := fs.Int("size", 64, "request and response size in bytes")
	gap := fs.Duration("gap", 2*time.Millisecond, "pause between connections")
	_ = fs.Parse(args)
	if err := resolve(addr); err != nil {
		return err
	}
	req := make([]byte, 1+*size)
	req[0] = 'E'
	res := make([]byte, *size)
	var d []time.Duration
	for i := 0; i < *warmup+*count; i++ {
		t := time.Now()
		c, err := net.DialTimeout("tcp", *addr, 3*time.Second)
		if err != nil {
			return err
		}
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Write(req); err != nil {
			c.Close()
			return err
		}
		if _, err := io.ReadFull(c, res); err != nil {
			c.Close()
			return err
		}
		el := time.Since(t)
		c.Close()
		if i >= *warmup {
			d = append(d, el)
		}
		time.Sleep(*gap)
	}
	return emit(struct {
		Kind string `json:"kind"`
		Size int    `json:"size"`
		Latency
	}{"connect", *size, summarize(d, 0)})
}

func stream(args []string) error {
	fs := flag.NewFlagSet("stream", flag.ExitOnError)
	addr := fs.String("addr", "", "server address")
	streams := fs.Int("streams", 1, "parallel connections")
	duration := fs.Duration("duration", 10*time.Second, "measured time")
	warmup := fs.Duration("warmup", time.Second, "time before measuring")
	_ = fs.Parse(args)
	if err := resolve(addr); err != nil {
		return err
	}
	var total atomic.Int64
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	errc := make(chan error, *streams)
	for range *streams {
		c, err := net.DialTimeout("tcp", *addr, 3*time.Second)
		if err != nil {
			return err
		}
		conns = append(conns, c)
		if _, err := c.Write([]byte{'D'}); err != nil {
			return err
		}
		go func() {
			buf := make([]byte, 256<<10)
			for {
				n, err := c.Read(buf)
				total.Add(int64(n))
				if err != nil {
					errc <- err
					return
				}
			}
		}()
	}
	wait := func(d time.Duration) error {
		select {
		case err := <-errc:
			return fmt.Errorf("stream ended early: %w", err)
		case <-time.After(d):
			return nil
		}
	}
	if err := wait(*warmup); err != nil {
		return err
	}
	b0, t0 := total.Load(), time.Now()
	if err := wait(*duration); err != nil {
		return err
	}
	b1, t1 := total.Load(), time.Now()
	sec := t1.Sub(t0).Seconds()
	return emit(map[string]any{
		"kind": "stream", "streams": *streams, "bytes": b1 - b0, "seconds": sec,
		"gbps": float64(b1-b0) * 8 / sec / 1e9,
	})
}

func load(args []string) error {
	fs := flag.NewFlagSet("load", flag.ExitOnError)
	addr := fs.String("addr", "", "server address")
	conns := fs.Int("conns", 50, "connections")
	rate := fs.Float64("rate", 200, "requests per second per connection")
	duration := fs.Duration("duration", 20*time.Second, "total time, including warmup")
	warmup := fs.Duration("warmup", 2*time.Second, "time before recording latency")
	size := fs.Int("size", 64, "request and response size in bytes")
	_ = fs.Parse(args)
	if err := resolve(addr); err != nil {
		return err
	}
	ecs := make([]*echoConn, *conns)
	for i := range ecs {
		e, err := dialEcho(*addr, *size)
		if err != nil {
			return err
		}
		defer e.c.Close()
		ecs[i] = e
	}
	start := time.Now()
	from, end := start.Add(*warmup), start.Add(*duration)
	period := time.Duration(float64(time.Second) / *rate)
	var mu sync.Mutex
	var all []time.Duration
	var errs []error
	var wg sync.WaitGroup
	for i, e := range ecs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Spread the connections over one period so the load is even.
			s := start.Add(period * time.Duration(i) / time.Duration(len(ecs)))
			d, _, err := paced(s, from, end, *rate, e.roundTrip)
			mu.Lock()
			defer mu.Unlock()
			all = append(all, d...)
			if err != nil {
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return emit(struct {
		Kind     string  `json:"kind"`
		Conns    int     `json:"conns"`
		Rate     float64 `json:"rate"`
		Achieved float64 `json:"achieved_rps"`
		Latency
	}{"load", *conns, *rate, float64(len(all)) / end.Sub(from).Seconds(), summarize(all, 0)})
}
