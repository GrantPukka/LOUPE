package parse

import (
	"bytes"
	"regexp"
	"strconv"
	"time"
)

func init() { Register(&nginxParser{}) }

// nginxParser reads the Nginx and Apache access log formats.
//
// Combined:
//
//	10.0.0.1 - alice [13/Aug/2026:14:02:00 +0000] "POST /api/cart HTTP/1.1" 200 547 "-" "curl/8.4.0"
//
// Common is the same without the trailing referer and user-agent, and a very
// widespread site variant inserts $server_name or $host in second position:
//
//	10.0.0.1 web-04 - alice [13/Aug/2026:14:02:00 +0000] "POST /api/cart HTTP/1.1" 200 547
//
// All three are handled by one expression, anchored on the bracketed date
// rather than on how many fields precede it. Counting fields was the older
// approach and it silently rejected the server-name variant outright, which on
// a real edge log means no status, no client and no path on the busiest source
// in the file.
type nginxParser struct{}

func (p *nginxParser) Name() string { return "nginx" }

// nginxRe matches common and combined in one pass.
//
// The bracketed timestamp is the distinctive part: no other format in this
// package writes a date that way, which is what makes detection reliable.
var nginxRe = regexp.MustCompile(
	`^(\S+)((?: \S+){2,3}) \[([^\]]+)\] "([^"]*)" (\d{3}) (\d+|-)` +
		`(?: "([^"]*)" "([^"]*)")?(.*)$`)

// requestRe splits the quoted request line into method, path, and protocol.
//
// It is separate because a malformed request line is common — scanners send
// junk — and a request that does not split must not fail the whole record.
var requestRe = regexp.MustCompile(`^(\S+) (\S+)(?: (\S+))?$`)

func (p *nginxParser) Detect(sample [][]byte) float64 {
	if len(sample) == 0 {
		return 0
	}

	var considered, matched int
	for _, line := range sample {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		considered++
		if nginxRe.Match(line) {
			matched++
		}
	}
	if considered == 0 {
		return 0
	}
	return float64(matched) / float64(considered)
}

func (p *nginxParser) Parse(line []byte) (Record, error) {
	m := nginxRe.FindSubmatch(line)
	if m == nil {
		return Record{}, ErrNoMatch
	}

	rec := Record{Fields: make(map[string]any, 10)}

	// The bracketed time carries its own offset, so the zone is known.
	if ts, zoned, ok := ParseTime(string(m[3]), time.UTC); ok {
		rec.Timestamp, rec.TimestampZoned = ts, zoned
	}

	rec.Fields["client"] = string(m[1])
	setNginxIdentity(rec.Fields, m[2])

	request := string(m[4])
	rec.Message = request
	if rm := requestRe.FindStringSubmatch(request); rm != nil {
		rec.Fields["method"] = rm[1]
		rec.Fields["path"] = rm[2]
		if rm[3] != "" {
			rec.Fields["protocol"] = rm[3]
		}
		// The path alone reads better in a timeline than the full request
		// line, and the method is one column away.
		rec.Message = rm[1] + " " + rm[2]
	}

	status, err := strconv.ParseInt(string(m[5]), 10, 64)
	if err == nil {
		rec.Fields["status"] = status
		rec.Level = levelForStatus(status)
	}

	if bytesSent := string(m[6]); bytesSent != "-" {
		if n, err := strconv.ParseInt(bytesSent, 10, 64); err == nil {
			rec.Fields["bytes"] = n
		}
	}

	if referer := string(m[7]); referer != "" && referer != "-" {
		rec.Fields["referer"] = referer
	}
	if agent := string(m[8]); agent != "" && agent != "-" {
		rec.Fields["agent"] = agent
	}

	// Sites routinely append their own key=value pairs to log_format —
	// rt=, upstream=, uct=, host= are in nginx's own documentation. Left
	// unread they are the difference between seeing a 502 and seeing which
	// upstream produced it, and before this they were the reason the logfmt
	// parser out-scored nginx on its own access log.
	addKeyValueTail(rec.Fields, m[9])

	return rec, nil
}

// setNginxIdentity assigns the two or three fields between the client address
// and the bracketed date.
//
// Two is the documented combined format: $remote_user preceded by the ident
// field nothing has written since the 1990s. Three means the site put
// $server_name or $host in front of them, which is the common multi-vhost
// setup. A hyphen is how nginx writes "absent", so it is not stored.
func setNginxIdentity(fields map[string]any, middle []byte) {
	parts := bytes.Fields(middle)
	if len(parts) == 3 {
		if server := string(parts[0]); server != "-" {
			fields["server"] = server
		}
		parts = parts[1:]
	}
	if len(parts) == 2 {
		if user := string(parts[1]); user != "-" {
			fields["user"] = user
		}
	}
}

// addKeyValueTail promotes a key=value section into fields without letting it
// overwrite anything the format itself already defined.
//
// Use it for a section that is known to be structured — a tail appended to a
// fixed format, a CEF extension. For a message that might just be prose, use
// addKeyValueMessage.
//
// A key that is not a plausible identifier is skipped rather than stored. It is
// the signature of a delimiter the splitter did not understand — a pipe, a URL,
// a timing tuple — and inventing a field named `dropped|5|src` would put noise
// into `loupe fields` for every reader afterwards.
func addKeyValueTail(fields map[string]any, tail []byte) {
	for _, pair := range namedPairs(tail) {
		putField(fields, pair.key, typed(pair.value))
	}
}

// addKeyValueMessage promotes a whole log message into fields, but only when
// the message really is structured data rather than prose.
//
// The test is that the pairs outnumber the bare words. auditd's
// `type=USER_ROLE_CHANGE actor=svc_deploy approved_by=NONE` is a record, and
// leaving it as text means approved_by=NONE is something to eyeball rather than
// filter on. systemd's `nginx.service: Main process exited, code=killed,
// status=9/KILL` is a sentence that happens to contain two pairs, and reading
// `status` out of it would collide a service exit code with every HTTP status
// in the file — turning a numeric column into text and breaking status:>=500
// for the whole ingest.
func addKeyValueMessage(fields map[string]any, message []byte) {
	pairs := parseLogfmt(bytes.TrimSpace(message))
	named := namedPairs(message)
	if len(named) < 2 || len(named) <= len(pairs)-len(named) {
		return
	}
	for _, pair := range named {
		putField(fields, pair.key, typed(pair.value))
	}
}

// namedPairs returns the pairs in a section whose keys could be field names.
func namedPairs(section []byte) []logfmtPair {
	section = bytes.TrimSpace(section)
	if len(section) == 0 {
		return nil
	}
	pairs := parseLogfmt(section)
	named := pairs[:0]
	for _, pair := range pairs {
		if pair.key != "" && fieldNameRe.MatchString(pair.key) {
			named = append(named, pair)
		}
	}
	return named
}

// fieldNameRe is the shape a key must have to become a field.
var fieldNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// levelForStatus derives a severity from the HTTP status code.
//
// Access logs carry no severity field, so this is inferred rather than read.
// It is worth doing: the whole cross-source pitch is watching a database fail,
// then the app error, then Nginx return 502s, and that only works if
// level:>=error catches all three. Without it a user has to know to write
// status:>=500 for one source and level:>=error for the others.
//
// The mapping is the conventional one and nothing else is guessed at: a 2xx or
// 3xx is info, not "success", and no level is invented for a status the
// expression did not capture.
func levelForStatus(status int64) string {
	switch {
	case status >= 500:
		return LevelError
	case status >= 400:
		return LevelWarn
	default:
		return LevelInfo
	}
}
