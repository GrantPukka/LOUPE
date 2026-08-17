package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GrantPukka/loupe/internal/schema"
)

// schemaTable records which fields were promoted, so a cached database restores
// the decision without re-sampling.
const schemaTable = `
CREATE TABLE IF NOT EXISTS loupe_schema (
    field     VARCHAR NOT NULL,
    column_   VARCHAR NOT NULL,
    sql_type  VARCHAR NOT NULL,
    present   BIGINT  NOT NULL,
    coverage  DOUBLE  NOT NULL
)`

// InferAndPromote samples the fields bag and gives the frequent fields real
// typed columns.
//
// Promoted fields stop being JSON extractions evaluated per row and become
// columnar scans, which is what the 10M-rows-in-500ms target needs.
//
// Promoted keys deliberately stay in the fields bag as well. Stripping them
// would need a per-row JSON rewrite for no benefit that matters: raw already
// holds a copy of everything, so free-text search is unaffected either way, and
// the promoted fields are by definition the small common ones.
func (s *DB) InferAndPromote(ctx context.Context, opts schema.Options) ([]schema.Promotion, []schema.Skip, error) {
	opts = opts.WithDefaults()

	samples, err := s.sampleFields(ctx, opts)
	if err != nil {
		return nil, nil, err
	}
	if len(samples) == 0 {
		return nil, nil, nil
	}

	promotions, skips := schema.Infer(samples, opts)
	if len(promotions) == 0 {
		return nil, skips, s.recordSchema(ctx, nil)
	}

	if err := s.applyPromotions(ctx, promotions); err != nil {
		return nil, nil, err
	}
	if err := s.recordSchema(ctx, promotions); err != nil {
		return nil, nil, err
	}

	return promotions, skips, nil
}

// sampleFields reads a stratified sample of the field bags.
//
// Sampling the head of the table would sample the first file. loupe's whole
// premise is a directory of unlike sources, and in the demo directory the first
// ten thousand records are all Nginx — so a head sample promotes Nginx's
// columns and nothing else, which is a confidently wrong schema.
//
// Taking an equal slice from each source instead means every format is seen,
// which is also what lets schema.Infer judge coverage per source.
//
// The sample is deterministic — the first rows of each source, not a random
// draw — because the promotion decision is cached, and a schema that varied
// between runs over identical files would be indefensible.
func (s *DB) sampleFields(ctx context.Context, opts schema.Options) ([]schema.Sample, error) {
	sources, err := s.countSources(ctx)
	if err != nil {
		return nil, err
	}
	if sources == 0 {
		return nil, nil
	}

	perSource := opts.SampleSize / sources
	if perSource < 1 {
		perSource = 1
	}

	rows, err := s.Query(ctx, `
		SELECT source, fields FROM (
			SELECT source, fields, row_number() OVER (PARTITION BY source ORDER BY seq) AS rn
			FROM logs
			WHERE fields IS NOT NULL
		)
		WHERE rn <= ?`, perSource)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var samples []schema.Sample
	for rows.Next() {
		var (
			source string
			raw    sql.NullString
		)
		if err := rows.Scan(&source, &raw); err != nil {
			return nil, fmt.Errorf("scan field bag: %w", err)
		}
		if !raw.Valid {
			continue
		}

		var bag map[string]any
		if err := json.Unmarshal([]byte(raw.String), &bag); err != nil {
			// A bag we cannot decode is a record we cannot learn from, but it
			// is not a reason to abandon inference.
			continue
		}
		samples = append(samples, schema.Sample{Source: source, Fields: normaliseNumbers(bag)})
	}

	return samples, rows.Err()
}

// countSources returns how many distinct logical sources were ingested.
func (s *DB) countSources(ctx context.Context) (int, error) {
	var n int
	row := s.QueryRow(ctx, `SELECT count(DISTINCT source) FROM logs`)
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count sources: %w", err)
	}
	return n, nil
}

// normaliseNumbers converts JSON numbers back to int64 where they are whole.
//
// encoding/json gives float64 for every number, which would make every integer
// field look fractional and promote to DOUBLE. The parsers stored these as
// int64 originally, so this restores what they meant.
func normaliseNumbers(bag map[string]any) map[string]any {
	for key, value := range bag {
		f, ok := value.(float64)
		if !ok {
			continue
		}
		if f == float64(int64(f)) {
			bag[key] = int64(f)
		}
	}
	return bag
}

// applyPromotions rebuilds the table with the new columns in a single pass.
//
// One CREATE TABLE AS rather than an ALTER plus UPDATE per column: the latter
// rewrites the whole table once per promoted field, which on a 32-column
// promotion is 32 full rewrites.
func (s *DB) applyPromotions(ctx context.Context, promotions []schema.Promotion) error {
	projections := make([]string, 0, len(promotions))
	for _, p := range promotions {
		// TRY_CAST so one unparseable value yields NULL for that row rather
		// than failing the rebuild for every row.
		projections = append(projections, fmt.Sprintf(
			`TRY_CAST(%s AS %s) AS %s`,
			jsonExtract(p.Field), p.Kind.SQLType(), quoteIdent(p.Column)))
	}

	// The field names come from keys already present in the ingested data, and
	// both the path literal and the identifier are escaped, so nothing here is
	// attacker-controlled query text. Placeholders are not accepted in a JSON
	// path or a column definition, which is why this is built as text.
	rebuild := fmt.Sprintf(
		`CREATE TABLE logs_promoted AS SELECT *, %s FROM logs`,
		strings.Join(projections, ", "))

	for _, stmt := range []string{
		`DROP TABLE IF EXISTS logs_promoted`,
		rebuild,
		`DROP TABLE logs`,
		`ALTER TABLE logs_promoted RENAME TO logs`,
	} {
		if err := s.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("promote fields: %w", err)
		}
	}

	return nil
}

// jsonExtract builds the extraction expression for a field name.
func jsonExtract(field string) string {
	return `fields->>'$."` + strings.ReplaceAll(field, `"`, `\"`) + `"'`
}

// quoteIdent wraps an identifier in double quotes, doubling any inside.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func (s *DB) recordSchema(ctx context.Context, promotions []schema.Promotion) error {
	if err := s.Exec(ctx, schemaTable); err != nil {
		return err
	}
	if err := s.Exec(ctx, `DELETE FROM loupe_schema`); err != nil {
		return err
	}

	for _, p := range promotions {
		err := s.Exec(ctx,
			`INSERT INTO loupe_schema VALUES (?, ?, ?, ?, ?)`,
			p.Field, p.Column, p.Kind.SQLType(), int64(p.Present), p.Coverage)
		if err != nil {
			return fmt.Errorf("record promoted schema: %w", err)
		}
	}
	return nil
}

// Promotions returns the fields promoted in this database.
//
// A cache hit reads this rather than re-sampling, so the query compiler and the
// HTTP schema endpoint see the same columns a cold run produced.
func (s *DB) Promotions(ctx context.Context) ([]schema.Promotion, error) {
	var exists bool
	row := s.QueryRow(ctx,
		`SELECT count(*) > 0 FROM information_schema.tables WHERE table_name = 'loupe_schema'`)
	if err := row.Scan(&exists); err != nil || !exists {
		return nil, nil
	}

	rows, err := s.Query(ctx,
		`SELECT field, column_, sql_type, present, coverage FROM loupe_schema ORDER BY present DESC, field`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []schema.Promotion
	for rows.Next() {
		var (
			p       schema.Promotion
			sqlType string
			present int64
		)
		if err := rows.Scan(&p.Field, &p.Column, &sqlType, &present, &p.Coverage); err != nil {
			return nil, fmt.Errorf("scan promoted schema: %w", err)
		}
		p.Present = int(present)
		p.Kind = kindFromSQL(sqlType)
		out = append(out, p)
	}
	return out, rows.Err()
}

func kindFromSQL(sqlType string) schema.Kind {
	switch strings.ToUpper(sqlType) {
	case "BIGINT":
		return schema.KindInt
	case "DOUBLE":
		return schema.KindFloat
	case "BOOLEAN":
		return schema.KindBool
	case "TIMESTAMP":
		return schema.KindTimestamp
	default:
		return schema.KindString
	}
}
