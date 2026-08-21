package parse

import (
	"bytes"
	"time"
)

func init() { Register(&criParser{}) }

// criParser reads the CRI log format written by containerd and CRI-O, which is
// what /var/log/pods and /var/log/containers hold on a modern Kubernetes node.
//
// Four space-separated parts:
//
//	2026-08-13T14:02:00.123456789Z stdout F the message
//	|                              |      | |
//	RFC3339Nano timestamp          stream tag message
//
// The tag is F for a complete line or P for a fragment of one that exceeded the
// runtime's buffer.
type criParser struct{}

func (p *criParser) Name() string { return "cri" }

func (p *criParser) Detect(sample [][]byte) float64 {
	var considered, matched int

	for _, line := range sample {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		considered++
		if _, ok := splitCRI(line); ok {
			matched++
		}
	}

	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

// criFields is one line taken apart.
type criFields struct {
	timestamp []byte
	stream    []byte
	tag       []byte
	message   []byte
}

// splitCRI takes a line apart, reporting whether it has the shape at all.
//
// The shape check is what detection runs on every sampled line, so it is done
// by splitting rather than by a regular expression: this is the hot path on a
// 4GB file and three SplitN calls are a great deal cheaper than a backtracking
// match.
func splitCRI(line []byte) (criFields, bool) {
	var out criFields

	parts := bytes.SplitN(line, []byte(" "), 4)
	if len(parts) < 3 {
		return out, false
	}

	out.timestamp, out.stream, out.tag = parts[0], parts[1], parts[2]
	if len(parts) == 4 {
		out.message = parts[3]
	}

	if !bytes.Equal(out.stream, []byte("stdout")) && !bytes.Equal(out.stream, []byte("stderr")) {
		return out, false
	}
	// The tag is F or P, optionally followed by colon-separated extensions the
	// spec reserves and nothing yet writes.
	if len(out.tag) == 0 || (out.tag[0] != 'F' && out.tag[0] != 'P') {
		return out, false
	}
	if len(out.tag) > 1 && out.tag[1] != ':' {
		return out, false
	}

	// The timestamp is the expensive check, so it goes last: a line that fails
	// the cheap shape tests never reaches it.
	if _, _, ok := ParseTime(string(out.timestamp), time.UTC); !ok {
		return out, false
	}
	return out, true
}

func (p *criParser) Parse(line []byte) (Record, error) {
	trimmed := bytes.TrimRight(line, "\r\n")

	fields, ok := splitCRI(trimmed)
	if !ok {
		return Record{}, ErrNoMatch
	}

	rec := Record{
		Message: string(fields.message),
		Fields:  map[string]any{"stream": string(fields.stream)},
	}

	if ts, zoned, ok := ParseTime(string(fields.timestamp), time.UTC); ok {
		rec.Timestamp, rec.TimestampZoned = ts, zoned
	}

	// A partial line is one the runtime split because it exceeded its read
	// buffer, so the record is a fragment rather than a whole log line.
	//
	// The fragments are kept as separate records and marked, rather than
	// stitched back together. Joining them needs to know that the *previous*
	// line was tagged P, and the Continuer interface asks the opposite
	// question — whether this line continues the last one — so making it fit
	// would mean widening the contribution surface for one format. CLAUDE.md
	// is explicit that the Parser interface does not grow to accommodate one
	// awkward case. Nothing is lost either way: partial:true finds every
	// fragment, and they sit adjacent on the timeline.
	if fields.tag[0] == 'P' {
		rec.Fields["partial"] = true
	}

	rec.Level = levelFromMessage(rec.Message)

	return rec, nil
}
