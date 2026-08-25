package render

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/GrantPukka/loupe/internal/store"
)

// json renders the whole result as one object, including the counts. The
// truncation flag is part of the payload rather than a side note, so a script
// consuming this cannot mistake a truncated page for the whole answer.
func (w *Writer) json(res store.Result) error {
	payload := struct {
		Columns   []string         `json:"columns"`
		Rows      []map[string]any `json:"rows"`
		Total     int64            `json:"total"`
		Truncated bool             `json:"truncated"`
		TookMS    float64          `json:"took_ms"`
	}{
		Columns:   res.Columns,
		Rows:      make([]map[string]any, 0, len(res.Rows)),
		Total:     res.Total,
		Truncated: res.Truncated,
		TookMS:    float64(res.Took.Microseconds()) / 1000,
	}

	localise := w.localisedColumns(res)
	for _, row := range res.Rows {
		payload.Rows = append(payload.Rows, w.rowMap(res.Columns, row, localise))
	}

	enc := json.NewEncoder(w.w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		return fmt.Errorf("encode json: %w", err)
	}
	return nil
}

// ndjson writes one object per line, for piping into jq.
func (w *Writer) ndjson(res store.Result) error {
	localise := w.localisedColumns(res)
	enc := json.NewEncoder(w.w)
	for _, row := range res.Rows {
		if err := enc.Encode(w.rowMap(res.Columns, row, localise)); err != nil {
			return fmt.Errorf("encode ndjson: %w", err)
		}
	}
	return nil
}

// raw writes the original lines, for grep compatibility.
//
// It needs the raw column, which is why the store always keeps it.
func (w *Writer) raw(res store.Result) error {
	idx := indexOf(res.Columns, "raw")
	if idx < 0 {
		return fmt.Errorf("--format raw needs the raw column; select it or use a filter query")
	}

	// Selecting line_no, raw and getting only raw looks like the projection was
	// ignored, and silently dropping a column the user asked for is exactly the
	// kind of quiet loss this tool is built not to do. The other columns are one
	// --format csv away.
	if dropped := otherColumns(res.Columns, idx); len(dropped) > 0 {
		fmt.Fprintf(os.Stderr,
			"--format raw prints the original line only; %s %s not shown "+
				"(use --format csv or ndjson to keep them).\n",
			strings.Join(dropped, ", "), plural(int64(len(dropped)), "is", "are"))
	}

	for _, row := range res.Rows {
		if _, err := fmt.Fprintln(w.w, w.value(row[idx], false)); err != nil {
			return err
		}
	}
	return nil
}

// otherColumns lists the selected columns raw output cannot show.
func otherColumns(cols []string, keep int) []string {
	var out []string
	for i, col := range cols {
		if i != keep {
			out = append(out, col)
		}
	}
	return out
}

func (w *Writer) csv(res store.Result) error {
	cw := csv.NewWriter(w.w)
	if err := cw.Write(res.Columns); err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}

	localise := w.localisedColumns(res)
	record := make([]string, len(res.Columns))
	for _, row := range res.Rows {
		for i := range res.Columns {
			if i < len(row) {
				record[i] = w.value(row[i], i < len(localise) && localise[i])
			} else {
				record[i] = ""
			}
		}
		if err := cw.Write(record); err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}

	cw.Flush()
	return cw.Error()
}

// rowMap converts a row to a JSON-friendly object.
//
// The fields column arrives as a JSON string from DuckDB and is re-embedded as
// an object rather than a quoted string, so that jq can reach into it.
func (w *Writer) rowMap(cols []string, row []any, localise []bool) map[string]any {
	out := make(map[string]any, len(cols))

	for i, col := range cols {
		if i >= len(row) {
			continue
		}
		val := row[i]

		switch v := val.(type) {
		case nil:
			out[col] = nil
			continue
		case time.Time:
			// RFC3339 in the display timezone, so a consumer sees the same
			// instant the table showed — but only where the value is an
			// instant at all. See Options.UserSQL.
			if i < len(localise) && localise[i] {
				out[col] = v.In(w.opts.Location).Format(time.RFC3339Nano)
			} else {
				out[col] = v.Format("2006-01-02T15:04:05.999999999")
			}
			continue
		case []byte:
			val = string(v)
		}

		if s, ok := val.(string); ok && col == "fields" {
			var nested any
			if err := json.Unmarshal([]byte(s), &nested); err == nil {
				out[col] = nested
				continue
			}
		}
		out[col] = val
	}

	return out
}
