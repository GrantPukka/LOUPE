package session

import (
	"context"
	"fmt"
	"strings"

	"github.com/GrantPukka/loupe/internal/pattern"
	"github.com/GrantPukka/loupe/internal/query"
)

// patternKey is the DSL term that selects a template.
const patternKey = "pattern"

// maxPatternCandidates bounds how many ids an ambiguity or suggestion message
// will list. Twenty is more than anyone reads and still a bounded query.
const maxPatternCandidates = 20

// UnknownPatternError names a template id that is not in the loaded data.
//
// An unmatched id is an error rather than an empty result, which is the
// opposite of how source: behaves — and deliberately so. A source name is
// something a user knows from outside the data, so "is nginx in here?" is a
// real question with "no" as a real answer. A template id only ever comes from
// a `loupe patterns` listing of this same data, so an id that is not present
// is a typo or a stale paste, never a question worth an empty table.
type UnknownPatternError struct {
	ID string
	// Near are ids that share a prefix with what was typed, which is what a
	// mistyped or truncated id looks like.
	Near []string
}

func (e *UnknownPatternError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "no template with id %q in this data", e.ID)

	if len(e.Near) > 0 {
		fmt.Fprintf(&sb, "\ndid you mean: %s", strings.Join(e.Near, ", "))
	}

	sb.WriteString("\nrun `loupe patterns` to list the templates in this data")
	return sb.String()
}

// AmbiguousPatternError reports a short id matching more than one template.
type AmbiguousPatternError struct {
	Prefix     string
	Candidates []string
	// More is how many further ids matched beyond those listed.
	More int
}

func (e *AmbiguousPatternError) Error() string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "template id %q is ambiguous: it matches %d templates",
		e.Prefix, len(e.Candidates)+e.More)
	fmt.Fprintf(&sb, "\ncandidates: %s", strings.Join(e.Candidates, ", "))
	if e.More > 0 {
		fmt.Fprintf(&sb, ", … and %d more", e.More)
	}
	sb.WriteString("\ntype more of the id to pick one")
	return sb.String()
}

// MalformedPatternError reports a value that is not shaped like a template id.
type MalformedPatternError struct {
	ID string
}

func (e *MalformedPatternError) Error() string {
	return fmt.Sprintf("template id %q is not a template id: "+
		"ids are hexadecimal, up to %d characters, as printed by `loupe patterns`\n"+
		"pattern: takes an id, not a template's text",
		e.ID, pattern.IDLength)
}

// resolvePatterns expands short template ids and rejects ones not in the data.
//
// This runs in the session rather than the compiler because it needs the
// database, and internal/query deliberately never touches one. It resolves
// with bounded lookups rather than by loading every id: a corpus where each
// line is unique has as many templates as records, and holding that list in
// memory to validate one term would be a poor trade.
func (s *Session) resolvePatterns(ctx context.Context, q query.Query) (query.Query, error) {
	out := query.Query{Terms: make([]query.Term, len(q.Terms))}
	copy(out.Terms, q.Terms)

	for i, term := range q.Terms {
		field, ok := term.(*query.FieldTerm)
		if !ok || !strings.EqualFold(field.Key, patternKey) {
			continue
		}
		// Only equality is resolved. A ~ match is asking for a substring by
		// definition, and a range comparison on a hash is meaningless but not
		// this function's business to refuse.
		if field.Op != query.OpEq {
			continue
		}

		resolved, err := s.resolvePatternValues(ctx, field.Values)
		if err != nil {
			return query.Query{}, err
		}
		if resolved == nil {
			continue
		}

		// A copy, because the caller keeps the parsed query for Explain and
		// for rendering the filter back. Rewriting a term in place would make
		// the query it reports differ from the one the user typed.
		replaced := *field
		replaced.Values = resolved
		out.Terms[i] = &replaced
	}

	return out, nil
}

// resolvePatternValues resolves each value of one pattern term, returning nil
// when nothing needed changing.
func (s *Session) resolvePatternValues(ctx context.Context, values []query.Value) ([]query.Value, error) {
	var out []query.Value
	changed := false

	for _, v := range values {
		// The existence tests are handled by the compiler and mean something
		// here: pattern:none finds records with no template at all.
		switch strings.ToLower(v.Text) {
		case "none", "*":
			out = append(out, v)
			continue
		}

		full, err := s.resolvePatternID(ctx, v.Text)
		if err != nil {
			return nil, err
		}

		next := v
		next.Text = full
		changed = changed || full != v.Text
		out = append(out, next)
	}

	if !changed {
		return nil, nil
	}
	return out, nil
}

// resolvePatternID expands one id, which may be a unique prefix of a full one.
//
// Prefix matching for the same reason git has short hashes: twelve hex
// characters is a lot to retype from a listing, and the first six are almost
// always unique. An ambiguous prefix errors and lists the candidates rather
// than silently picking one.
func (s *Session) resolvePatternID(ctx context.Context, id string) (string, error) {
	if !plausiblePatternID(id) {
		return "", &MalformedPatternError{ID: id}
	}

	matches, err := s.patternIDsWithPrefix(ctx, id, maxPatternCandidates+1)
	if err != nil {
		return "", err
	}

	switch {
	case len(matches) == 1:
		return matches[0], nil

	case len(matches) > 1:
		// An exact hit wins over the longer ids it is a prefix of. Without
		// this, a full id would be unusable whenever another template's id
		// happened to extend it.
		for _, m := range matches {
			if m == id {
				return m, nil
			}
		}
		more := 0
		if len(matches) > maxPatternCandidates {
			matches, more = matches[:maxPatternCandidates], len(matches)-maxPatternCandidates
		}
		return "", &AmbiguousPatternError{Prefix: id, Candidates: matches, More: more}
	}

	// Nothing matched. A mistyped id usually has a good prefix and a bad tail,
	// so a shorter prefix is where the intended template will be.
	return "", &UnknownPatternError{ID: id, Near: s.nearPatternIDs(ctx, id)}
}

// nearPatternIDs looks for templates whose id starts like the one typed.
//
// Best effort: this only builds an error message, so a failed lookup means no
// suggestions rather than a different error replacing the real one.
func (s *Session) nearPatternIDs(ctx context.Context, id string) []string {
	const minPrefix = 4

	for cut := len(id) - 1; cut >= minPrefix; cut-- {
		matches, err := s.patternIDsWithPrefix(ctx, id[:cut], 5)
		if err != nil {
			return nil
		}
		if len(matches) > 0 {
			return matches
		}
	}
	return nil
}

// patternIDsWithPrefix returns up to limit template ids beginning with prefix.
func (s *Session) patternIDsWithPrefix(ctx context.Context, prefix string, limit int) ([]string, error) {
	// The prefix is checked to be hexadecimal before it gets here, so it holds
	// no LIKE metacharacters. It is still passed as a parameter rather than
	// interpolated, because the rule in CLAUDE.md is that user input is never
	// concatenated into SQL and a rule with exceptions is not a rule.
	rows, err := s.DB.Query(ctx,
		`SELECT DISTINCT pattern_id FROM logs
		 WHERE pattern_id IS NOT NULL AND starts_with(pattern_id, ?)
		 ORDER BY pattern_id LIMIT `+fmt.Sprint(limit), prefix)
	if err != nil {
		return nil, fmt.Errorf("look up template id: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan template id: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate template ids: %w", err)
	}
	return out, nil
}

// plausiblePatternID reports whether a value is shaped like a template id.
//
// Checked before any lookup so that `pattern:timed out` is told what a pattern
// id is, rather than being reported as an id that happens not to exist. The
// two mistakes need different corrections.
func plausiblePatternID(id string) bool {
	if id == "" || len(id) > pattern.IDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
