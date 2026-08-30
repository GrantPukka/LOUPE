package parse

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
)

// DetectSampleLines is how many lines from the head of a source are shown to
// each parser. Large enough to see past a header or a burst of one unusual
// format, small enough to stay cheap on a 4GB file.
const DetectSampleLines = 200

// The confidence ceilings a parser claims, highest first.
//
// Detect returns "how specifically do I claim this line", not "how sure am I
// that it parses". That distinction only pays off if the generic readers sit
// below the specific ones by a clear margin, because mixedParser resolves a tie
// by parser name — so without a ladder, whether nginx or logfmt reads an nginx
// access log with a `rt=…` tail comes down to the letter l sorting before n.
//
// Every gap is at least Ambiguous's tenth, so a parser that wins also wins
// visibly rather than being reported as a coin flip.
//
//	1.00  a parser that recognises the format itself — nginx, postgres, redis
//	0.85  a frame around someone else's payload, or generic JSON
//	0.75  generic key=value, which almost any format's tail also looks like
//	0.01  the fallback, which claims everything and reads nothing
const (
	frameCeiling  = 0.85
	genericKVCeil = 0.75
)

// Detection is the outcome of choosing a parser for a source.
type Detection struct {
	Parser Parser
	// Confidence is the winning parser's score, 0.0-1.0.
	Confidence float64
	// Runner is the next best parser and its score, for reporting a close call.
	Runner     Parser
	RunnerUp   float64
	SampleSize int
}

// Ambiguous reports whether the winner beat the runner-up by so little that the
// choice was close to arbitrary. Callers surface this rather than silently
// picking one.
func (d Detection) Ambiguous() bool {
	return d.Runner != nil && d.Confidence-d.RunnerUp < 0.1
}

// Detect picks the parser with the highest confidence over a sample of lines.
//
// Ties break by parser name, since All returns parsers sorted, which keeps
// detection deterministic across runs. The fallback parser always scores just
// above zero, so a source is never left without a parser.
func Detect(sample [][]byte) Detection {
	d := Detection{SampleSize: len(sample)}

	for _, p := range All() {
		score := p.Detect(sample)
		switch {
		case d.Parser == nil || score > d.Confidence:
			d.Runner, d.RunnerUp = d.Parser, d.Confidence
			d.Parser, d.Confidence = p, score
		case score > d.RunnerUp:
			d.Runner, d.RunnerUp = p, score
		}
	}

	return d
}

// SampleLines reads up to DetectSampleLines non-blank lines from r for
// detection.
//
// Blank lines are skipped because damaged files contain many and they tell no
// parser anything. Overlong lines are truncated rather than dropped, since a
// truncated line still reveals a format.
func SampleLines(r io.Reader, limit int) ([][]byte, error) {
	if limit <= 0 {
		limit = DetectSampleLines
	}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), MaxLineBytes)

	var sample [][]byte
	for len(sample) < limit && sc.Scan() {
		line := bytes.TrimRight(sc.Bytes(), "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		sample = append(sample, append([]byte(nil), line...))
	}

	if err := sc.Err(); err != nil && err != bufio.ErrTooLong {
		return sample, fmt.Errorf("sample lines: %w", err)
	}
	return sample, nil
}
