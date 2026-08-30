package session

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// SystemLocation resolves the local zone to a named one.
//
// time.Local stringifies as "Local", which tells a user nothing and cannot be
// pasted into a ticket. docs/FILTER-DSL.md section 2.3 requires the active
// timezone be visible and unambiguous, so dig out the real name and only fall
// back to time.Local when the system will not say.
func SystemLocation() *time.Location {
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			return loc
		}
	}

	// /etc/localtime is a symlink into the zoneinfo tree on Linux and macOS.
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			name := target[i+len("zoneinfo/"):]
			if loc, err := time.LoadLocation(name); err == nil {
				return loc
			}
		}
	}

	return time.Local
}

// ParseLocation resolves the display timezone from the two flags that set it.
//
// One timezone for the whole session, defaulting to the system zone, and always
// stated on screen. A user must never have to guess whether the times they are
// reading are theirs or the server's.
func ParseLocation(utc bool, tz string) (*time.Location, error) {
	switch {
	case utc && tz != "":
		return nil, fmt.Errorf("--utc and --tz are mutually exclusive")
	case utc:
		return time.UTC, nil
	case tz != "":
		loc, err := time.LoadLocation(tz)
		if err != nil {
			return nil, fmt.Errorf(
				"unknown timezone %q: try a name from the tz database, e.g. Europe/London or UTC", tz)
		}
		return loc, nil
	default:
		return SystemLocation(), nil
	}
}

// ParseSourceZones reads --source-tz values.
//
// Two shapes: a bare zone applying to every source, and source:zone naming one.
// Both may be given, and the named one wins.
//
// The prefix is a source name — what `loupe sources` lists — not a format name.
// store.checkSourceZones rejects one that matches no source, because a prefix
// that silently matches nothing is worse than no flag at all.
func ParseSourceZones(values []string) (map[string]*time.Location, error) {
	if len(values) == 0 {
		return nil, nil
	}

	out := map[string]*time.Location{}
	for _, v := range values {
		name, zone := "", v
		if i := strings.LastIndex(v, ":"); i > 0 {
			name, zone = v[:i], v[i+1:]
		}

		loc, err := time.LoadLocation(zone)
		if err != nil {
			return nil, fmt.Errorf("unknown timezone %q in --source-tz %q: "+
				"use a tz database name, e.g. --source-tz=UTC or --source-tz=<source>:Europe/London",
				zone, v)
		}
		out[name] = loc
	}
	return out, nil
}
