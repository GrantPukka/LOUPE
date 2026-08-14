// Command blaster generates realistic, messy, multi-format log files for
// testing and demoing loupe.
//
// It is deliberately stdlib-only and CGO-free so it cross-compiles anywhere and
// can be run in CI to regenerate fixtures.
//
//	go run ./cmd/blaster -out ./demo -scenario incident
//	go run ./cmd/blaster -out ./demo -follow -rate 40
//	go run ./cmd/blaster -out ./testdata/mixed -seed 7 -duration 5m -malform 0.02
//
// The point is not volume. The point is that the six emitted files are in six
// different formats, are causally correlated with each other, and contain the
// specific kinds of mess that break parsers.
package main

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------- config

type config struct {
	out      string
	scenario string
	duration time.Duration
	rate     float64
	seed     int64
	malform  float64
	follow   bool
	rotate   bool
}

func main() {
	var c config
	flag.StringVar(&c.out, "out", "./demo", "output directory")
	flag.StringVar(&c.scenario, "scenario", "incident",
		"steady | incident | deploy-regression | quiet")
	flag.DurationVar(&c.duration, "duration", 18*time.Minute, "span of simulated time")
	flag.Float64Var(&c.rate, "rate", 12, "baseline requests per simulated second")
	flag.Int64Var(&c.seed, "seed", 42, "rng seed; same seed gives byte-identical output")
	flag.Float64Var(&c.malform, "malform", 0.015,
		"fraction of lines that are truncated, invalid, or otherwise broken")
	flag.BoolVar(&c.follow, "follow", false, "write in real time instead of all at once")
	flag.BoolVar(&c.rotate, "rotate", true, "also emit rotated .log.1 and .log.2.gz files")
	flag.Parse()

	if err := run(c); err != nil {
		fmt.Fprintln(os.Stderr, "blaster:", err)
		os.Exit(1)
	}
}

func run(c config) error {
	rng := rand.New(rand.NewSource(c.seed))
	if err := os.MkdirAll(c.out, 0o755); err != nil {
		return fmt.Errorf("create out dir: %w", err)
	}

	end := time.Date(2026, 8, 13, 14, 20, 0, 0, time.UTC)
	start := end.Add(-c.duration)

	events := generate(rng, c, start, end)
	sort.Slice(events, func(i, j int) bool { return events[i].t.Before(events[j].t) })

	if c.follow {
		return stream(c, events, start, end)
	}
	return writeAll(c, rng, events)
}

// ---------------------------------------------------------------- model

// An entry is one line destined for one file. Everything upstream of the
// formatters works in this shape so that correlation logic never has to think
// about text.
type entry struct {
	t       time.Time
	sink    string // logical file, e.g. "checkout-api"
	level   string
	msg     string
	fields  map[string]any
	stack   []string // extra continuation lines, for multi-line formats
	broken  string   // if set, emit this raw string instead (malformed line)
	traceID string
}

// sinks maps a logical source to its file name and wire format.
var sinks = []struct {
	name, file, format string
}{
	{"checkout-api", "checkout-api.log", "jsonl"},
	{"auth-svc", "auth-svc.log", "logfmt"},
	{"nginx", "access.log", "nginx-combined"},
	{"payment-worker", "payment-worker.log", "log4j"},
	{"postgres", "postgresql.log", "postgres"},
	{"host", "syslog", "rfc5424"},
}

// ---------------------------------------------------------------- generation

func generate(rng *rand.Rand, c config, start, end time.Time) []entry {
	var out []entry
	span := end.Sub(start)

	// The incident window sits at 60-72% through the run, so there is plenty of
	// calm on either side for the demo to establish a baseline.
	incStart := start.Add(time.Duration(float64(span) * 0.60))
	incEnd := start.Add(time.Duration(float64(span) * 0.72))

	total := int(c.rate * span.Seconds())
	for i := 0; i < total; i++ {
		t := start.Add(time.Duration(rng.Float64() * float64(span)))
		degraded := c.scenario == "incident" && t.After(incStart) && t.Before(incEnd)
		out = append(out, request(rng, t, degraded)...)
	}

	// Background chatter that is not request-driven.
	for i := 0; i < total/8; i++ {
		t := start.Add(time.Duration(rng.Float64() * float64(span)))
		out = append(out, chatter(rng, t)...)
	}

	if c.scenario == "incident" {
		out = append(out, rootCause(rng, incStart)...)
	}
	if c.scenario == "deploy-regression" {
		out = append(out, deploy(rng, start.Add(span/2))...)
	}

	// Break some lines on purpose. This is the whole reason the tool exists:
	// clean synthetic fixtures hide exactly the bugs that matter.
	for i := range out {
		if rng.Float64() < c.malform {
			out[i].broken = mangle(rng, out[i])
		}
	}
	return out
}

// request models one user request fanning out across services, sharing a trace
// id so that cross-source correlation can actually be demonstrated.
func request(rng *rand.Rand, t time.Time, degraded bool) []entry {
	trace := fmt.Sprintf("%08x", rng.Uint32())
	user := fmt.Sprintf("u_%04d", 1000+rng.Intn(9000))
	path := pick(rng, []string{"/api/checkout", "/api/cart", "/api/orders/2291",
		"/api/session", "/healthz"})

	status, level, lat := 200, "info", 40+rng.Intn(180)
	msg := "request completed"
	if degraded {
		switch {
		case rng.Float64() < 0.55:
			status, level, lat = 502, "error", 3000+rng.Intn(2000)
			msg = "upstream timeout contacting payments"
		case rng.Float64() < 0.35:
			status, level, lat = 429, "warn", 200+rng.Intn(400)
			msg = "rate limited by upstream"
		}
	} else if rng.Float64() < 0.04 {
		status, level, lat = 500, "error", 800+rng.Intn(1200)
		msg = pick(rng, []string{"failed to reserve stock", "invalid signature on webhook"})
	}

	es := []entry{
		{
			t: t, sink: "nginx", level: level, traceID: trace,
			msg: path,
			fields: map[string]any{
				"client": fmt.Sprintf("10.0.%d.%d", rng.Intn(4), rng.Intn(255)),
				"method": "POST", "path": path, "status": status,
				"bytes": 200 + rng.Intn(4000),
				"agent": pick(rng, []string{"curl/8.4.0", "checkout-web/2.1",
					"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"}),
			},
		},
		{
			t:    t.Add(time.Duration(rng.Intn(30)) * time.Millisecond),
			sink: "checkout-api", level: level, msg: msg, traceID: trace,
			fields: map[string]any{
				"trace_id": trace, "user_id": user, "path": path,
				"status": status, "latency_ms": lat, "region": pick(rng,
					[]string{"eu-west-1", "us-east-1", "ap-south-1"}),
			},
		},
	}

	if rng.Float64() < 0.4 {
		es = append(es, entry{
			t:    t.Add(time.Duration(rng.Intn(15)) * time.Millisecond),
			sink: "auth-svc", level: "info", msg: "token validated", traceID: trace,
			fields: map[string]any{"trace_id": trace, "user_id": user,
				"issuer": "auth.internal", "ttl_s": 3600},
		})
	}

	// Payment worker errors carry a Java stack trace, which is the multi-line
	// case every log tool gets wrong at least once.
	if degraded && rng.Float64() < 0.5 {
		es = append(es, entry{
			t:    t.Add(time.Duration(40+rng.Intn(80)) * time.Millisecond),
			sink: "payment-worker", level: "error", traceID: trace,
			msg: "PaymentGatewayException: read timed out after 3000ms",
			stack: []string{
				"\tat com.acme.pay.GatewayClient.charge(GatewayClient.java:214)",
				"\tat com.acme.pay.ChargeHandler.handle(ChargeHandler.java:88)",
				"\tat com.acme.pay.Worker.consume(Worker.java:141)",
				"\tCaused by: java.net.SocketTimeoutException: Read timed out",
				"\t\tat java.base/java.net.SocketInputStream.read(SocketInputStream.java:171)",
				"\t\t... 14 more",
			},
			fields: map[string]any{"trace_id": trace, "attempt": 1 + rng.Intn(3)},
		})
	}
	return es
}

func chatter(rng *rand.Rand, t time.Time) []entry {
	switch rng.Intn(4) {
	case 0:
		return []entry{{t: t, sink: "postgres", level: "info",
			msg: fmt.Sprintf("duration: %d.%03d ms  statement: SELECT * FROM orders WHERE user_id = $1",
				rng.Intn(400), rng.Intn(999)),
			fields: map[string]any{"pid": 20000 + rng.Intn(900), "db": "checkout"}}}
	case 1:
		return []entry{{t: t, sink: "host", level: "info", msg: "session opened for user deploy",
			fields: map[string]any{"app": "sshd", "pid": 1000 + rng.Intn(9000)}}}
	case 2:
		return []entry{{t: t, sink: "auth-svc", level: "debug", msg: "refreshing jwks cache",
			fields: map[string]any{"keys": 3, "next_refresh_s": 300}}}
	default:
		return []entry{{t: t, sink: "checkout-api", level: "debug",
			msg:    "evaluating feature flag",
			fields: map[string]any{"flag": "new_checkout_flow", "enabled": rng.Intn(2) == 1}}}
	}
}

// rootCause emits the actual cause of the incident, a few seconds before the
// symptoms start. Finding this by dragging the timeline back from the error
// spike is the demo.
func rootCause(rng *rand.Rand, at time.Time) []entry {
	t := at.Add(-6 * time.Second)
	return []entry{
		{t: t, sink: "postgres", level: "warn",
			msg:    "connection pool exhausted, 100 of 100 connections in use",
			fields: map[string]any{"pid": 20001, "db": "checkout"}},
		{t: t.Add(900 * time.Millisecond), sink: "postgres", level: "error",
			msg:    "FATAL: remaining connection slots are reserved for superusers",
			fields: map[string]any{"pid": 20044, "db": "checkout"}},
		{t: t.Add(2 * time.Second), sink: "host", level: "warn",
			msg:    "memory cgroup near limit: 3.8G of 4.0G",
			fields: map[string]any{"app": "kernel", "pid": 0}},
		{t: t.Add(4 * time.Second), sink: "payment-worker", level: "warn",
			msg:    "HikariPool-1 - Connection is not available, request timed out after 30000ms",
			fields: map[string]any{"pool": "HikariPool-1", "active": 100}},
	}
}

func deploy(rng *rand.Rand, at time.Time) []entry {
	return []entry{
		{t: at, sink: "host", level: "info", msg: "deployed checkout-api v2.14.0",
			fields: map[string]any{"app": "deployer", "pid": 4412}},
		{t: at.Add(3 * time.Second), sink: "checkout-api", level: "info",
			msg: "server listening", fields: map[string]any{"version": "2.14.0", "port": 8080}},
	}
}

// mangle returns a deliberately broken rendering of an entry.
func mangle(rng *rand.Rand, e entry) string {
	line := formatFor(e.sink)(e)
	switch rng.Intn(5) {
	case 0: // truncated mid-line, as happens when a process is killed
		if len(line) > 30 {
			return line[:20+rng.Intn(len(line)-25)]
		}
		return line
	case 1: // interleaved write from another thread
		return line[:len(line)/2] + "\x00\x00" + line[len(line)/2:]
	case 2: // JSON with an unescaped newline inside a string value
		return strings.Replace(line, "request completed", "request\ncompleted", 1)
	case 3: // blank line
		return ""
	default: // a line from an entirely unrelated format leaking into the file
		return "2026-08-13 14:11:02 [notice] 1#1: signal 17 (SIGCHLD) received from 812"
	}
}

// ---------------------------------------------------------------- formatters

type formatter func(entry) string

func formatFor(sink string) formatter {
	switch sink {
	case "checkout-api":
		return fmtJSONL
	case "auth-svc":
		return fmtLogfmt
	case "nginx":
		return fmtNginx
	case "payment-worker":
		return fmtLog4j
	case "postgres":
		return fmtPostgres
	default:
		return fmtSyslog
	}
}

func fmtJSONL(e entry) string {
	m := map[string]any{
		"ts":    e.t.UTC().Format(time.RFC3339Nano),
		"level": e.level,
		"msg":   e.msg,
	}
	for k, v := range e.fields {
		m[k] = v
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func fmtLogfmt(e entry) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "ts=%s level=%s msg=%q",
		e.t.UTC().Format(time.RFC3339), e.level, e.msg)
	for _, k := range sortedKeys(e.fields) {
		fmt.Fprintf(&sb, " %s=%v", k, e.fields[k])
	}
	return sb.String()
}

// Nginx combined, which uses a timestamp format shared with nothing else on
// earth and is a good test of layout detection.
func fmtNginx(e entry) string {
	return fmt.Sprintf(`%s - - [%s] "%s %s HTTP/1.1" %v %v "-" "%s"`,
		e.fields["client"],
		e.t.Format("02/Jan/2006:15:04:05 -0700"),
		e.fields["method"], e.fields["path"],
		e.fields["status"], e.fields["bytes"], e.fields["agent"])
}

func fmtLog4j(e entry) string {
	head := fmt.Sprintf("%s [worker-%d] %s c.a.p.ChargeHandler - %s",
		e.t.Format("2006-01-02 15:04:05.000"),
		1+len(e.msg)%4, strings.ToUpper(e.level), e.msg)
	if len(e.stack) == 0 {
		return head
	}
	return head + "\n" + strings.Join(e.stack, "\n")
}

func fmtPostgres(e entry) string {
	return fmt.Sprintf("%s [%v] %s: %s",
		e.t.Format("2006-01-02 15:04:05.000 MST"),
		e.fields["pid"], strings.ToUpper(e.level), e.msg)
}

// RFC5424 syslog.
func fmtSyslog(e entry) string {
	pri := map[string]int{"debug": 15, "info": 14, "warn": 12, "error": 11}[e.level]
	if pri == 0 {
		pri = 14
	}
	app := "kernel"
	if v, ok := e.fields["app"].(string); ok {
		app = v
	}
	return fmt.Sprintf("<%d>1 %s host-01 %s %v - - %s",
		pri, e.t.UTC().Format(time.RFC3339), app, e.fields["pid"], e.msg)
}

// ---------------------------------------------------------------- output

type manifest struct {
	Scenario string       `json:"scenario"`
	Seed     int64        `json:"seed"`
	Files    []fileReport `json:"files"`
}

type fileReport struct {
	File    string `json:"file"`
	Format  string `json:"format"`
	Lines   int    `json:"lines"`
	Records int    `json:"records"` // logical records; differs where lines wrap
	Broken  int    `json:"broken"`  // lines a parser is expected to reject
}

func writeAll(c config, rng *rand.Rand, events []entry) error {
	files := map[string]*os.File{}
	reps := map[string]*fileReport{}

	for _, s := range sinks {
		f, err := os.Create(filepath.Join(c.out, s.file))
		if err != nil {
			return fmt.Errorf("create %s: %w", s.file, err)
		}
		defer f.Close()
		files[s.name] = f
		reps[s.name] = &fileReport{File: s.file, Format: s.format}
	}

	for _, e := range events {
		f, ok := files[e.sink]
		if !ok {
			continue
		}
		r := reps[e.sink]
		text := e.broken
		if e.broken == "" {
			text = formatFor(e.sink)(e)
		} else {
			r.Broken++
		}
		if _, err := io.WriteString(f, text+"\n"); err != nil {
			return fmt.Errorf("write %s: %w", e.sink, err)
		}
		r.Records++
		r.Lines += strings.Count(text, "\n") + 1
	}

	if c.rotate {
		if err := writeRotated(c, rng); err != nil {
			return err
		}
	}

	m := manifest{Scenario: c.scenario, Seed: c.seed}
	for _, s := range sinks {
		m.Files = append(m.Files, *reps[s.name])
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(c.out, "manifest.json"), b, 0o644); err != nil {
		return err
	}

	fmt.Printf("wrote %d files to %s\n", len(sinks)+3, c.out)
	for _, r := range m.Files {
		fmt.Printf("  %-22s %-16s %6d records  %3d broken\n",
			r.File, r.Format, r.Records, r.Broken)
	}
	fmt.Printf("\n  loupe %s --ui\n", c.out)
	return nil
}

// writeRotated emits older, rotated copies so directory walking, ordering, and
// gzip handling all get exercised.
func writeRotated(c config, rng *rand.Rand) error {
	older := time.Date(2026, 8, 13, 13, 0, 0, 0, time.UTC)
	var lines []string
	for i := 0; i < 400; i++ {
		e := request(rng, older.Add(time.Duration(i)*time.Second), false)[0]
		lines = append(lines, fmtNginx(e))
	}
	plain := strings.Join(lines[:200], "\n") + "\n"
	if err := os.WriteFile(filepath.Join(c.out, "access.log.1"), []byte(plain), 0o644); err != nil {
		return err
	}
	f, err := os.Create(filepath.Join(c.out, "access.log.2.gz"))
	if err != nil {
		return err
	}
	defer f.Close()
	zw := gzip.NewWriter(f)
	if _, err := io.WriteString(zw, strings.Join(lines[200:], "\n")+"\n"); err != nil {
		return err
	}
	return zw.Close()
}

// stream writes entries in real time, for testing tail and live-follow paths.
func stream(c config, events []entry, start, end time.Time) error {
	files := map[string]*os.File{}
	for _, s := range sinks {
		f, err := os.OpenFile(filepath.Join(c.out, s.file),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		files[s.name] = f
	}
	fmt.Printf("following into %s, ctrl-c to stop\n", c.out)

	wall := time.Now()
	for _, e := range events {
		// Compress simulated time into real time at 60x.
		target := wall.Add(e.t.Sub(start) / 60)
		if d := time.Until(target); d > 0 {
			time.Sleep(d)
		}
		text := e.broken
		if text == "" {
			text = formatFor(e.sink)(e)
		}
		if _, err := io.WriteString(files[e.sink], text+"\n"); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- util

func pick[T any](rng *rand.Rand, xs []T) T { return xs[rng.Intn(len(xs))] }

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
