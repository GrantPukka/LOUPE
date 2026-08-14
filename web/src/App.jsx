import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { LIST_COLUMNS, getHistogram, getSchema, runQuery } from './api.js';
import { Histogram } from './Histogram.jsx';
import { Rows } from './Rows.jsx';
import { number, sourceColour, withoutTimeTerms } from './format.js';

// One page of records. Scrolling fetches the next.
const PAGE = 300;

// Typing pause before a query runs. Long enough not to query on every
// keystroke, short enough that the list feels attached to the box.
const DEBOUNCE_MS = 220;

export function App() {
  const [schema, setSchema] = useState(null);
  const [filter, setFilter] = useState('');
  const [applied, setApplied] = useState('');

  const [result, setResult] = useState(null);
  const [hist, setHist] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  const input = useRef(null);
  const generation = useRef(0);

  useEffect(() => {
    getSchema().then(setSchema).catch((e) => setError(e.message));
  }, []);

  // Debounce the filter, so a query runs when typing pauses rather than on
  // every keystroke.
  useEffect(() => {
    const timer = setTimeout(() => setApplied(filter.trim()), DEBOUNCE_MS);
    return () => clearTimeout(timer);
  }, [filter]);

  const load = useCallback(async (expression) => {
    // Responses can arrive out of order after a fast edit. Only the newest
    // query may write to the screen, or the list ends up showing the results
    // of a filter the user has already changed.
    const mine = ++generation.current;
    setBusy(true);

    try {
      const [records, histogram] = await Promise.all([
        runQuery({ filter: expression, limit: PAGE, columns: LIST_COLUMNS }),
        getHistogram({ filter: expression, buckets: 90 }),
      ]);

      if (mine !== generation.current) return;

      setResult(records);
      setHist(histogram);
      setError(null);
    } catch (e) {
      if (mine !== generation.current) return;
      setError(e.message);
    } finally {
      if (mine === generation.current) setBusy(false);
    }
  }, []);

  useEffect(() => {
    load(applied);
  }, [applied, load]);

  const loadMore = useCallback(async () => {
    if (!result || busy || result.rows.length >= result.total) return;

    const mine = generation.current;
    setBusy(true);
    try {
      const next = await runQuery({
        filter: applied,
        limit: PAGE,
        offset: result.rows.length,
        columns: LIST_COLUMNS,
      });
      if (mine !== generation.current) return;

      setResult((prev) =>
        prev ? { ...prev, rows: [...prev.rows, ...next.rows] } : next,
      );
    } catch {
      // Leave what is already loaded on screen.
    } finally {
      if (mine === generation.current) setBusy(false);
    }
  }, [applied, busy, result]);

  // Keyboard: / focuses the filter, Escape clears it.
  useEffect(() => {
    const onKey = (e) => {
      if (e.key === '/' && document.activeElement !== input.current) {
        e.preventDefault();
        input.current?.focus();
      }
      if (e.key === 'Escape' && document.activeElement === input.current) {
        setFilter('');
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  /** A timeline drag replaces any existing time term with the dragged range. */
  const onRange = useCallback((term) => {
    setFilter((current) => {
      const base = withoutTimeTerms(current);
      const next = term ? `${base} ${term}`.trim() : base;
      return next;
    });
  }, []);

  const toggleSource = useCallback((name) => {
    setFilter((current) => {
      const exclude = `-source:${name}`;
      const terms = (current || '').split(/\s+/).filter(Boolean);

      return terms.includes(exclude)
        ? terms.filter((t) => t !== exclude).join(' ')
        : [...terms, exclude].join(' ');
    });
  }, []);

  const timeZone = schema?.timezone ?? 'UTC';
  const excluded = new Set(
    (filter.match(/-source:(\S+)/g) ?? []).map((t) => t.slice('-source:'.length)),
  );

  return (
    <>
      <Header schema={schema} timeZone={timeZone} />

      <div class="sources">
        {groupSources(schema?.sources).map((s) => (
          <button
            class={`chip ${excluded.has(s.name) ? 'off' : ''}`}
            key={s.name}
            style={{ '--src': sourceColour(s.name) }}
            title={s.title}
            onClick={() => toggleSource(s.name)}
          >
            <span>{s.name}</span>
            <span class="fmt">{s.format}</span>
            <span class="n">{number(s.records)}</span>
            {s.assumed && <span class="assumed">tz?</span>}
          </button>
        ))}
      </div>

      <div class="filterbar">
        <span class="chev">›</span>
        <input
          ref={input}
          value={filter}
          autocomplete="off"
          spellcheck={false}
          placeholder="try:  level:error   ·   trace_id:a91c40f2   ·   status:>=500   ·   -source:nginx   ·   last:15m"
          onInput={(e) => setFilter(e.currentTarget.value)}
        />
        {busy && <span class="busy">…</span>}
        {filter && (
          <button class="clear" onClick={() => setFilter('')} title="Escape">
            clear
          </button>
        )}
      </div>

      {error && <div class="error-bar">{error}</div>}

      <Histogram hist={hist} timeZone={timeZone} onRange={onRange} />

      <div class="colhead">
        <span class="c-ts">time</span>
        <span class="c-lvl">level</span>
        <span class="c-src">source</span>
        <span class="c-msg">message</span>
      </div>

      <Rows
        rows={result?.rows ?? []}
        columns={result?.columns ?? []}
        timeZone={timeZone}
        filter={filter}
        onFilter={setFilter}
        onLoadMore={loadMore}
        hasMore={!!result && result.rows.length < result.total}
        empty={emptyMessage(result, error)}
      />

      <Footer result={result} hist={hist} schema={schema} />
    </>
  );
}

/**
 * Collapse the per-file source list into one chip per logical source.
 *
 * A rotation group is three files and one source: access.log, access.log.1,
 * and access.log.2.gz are all `access`, and -source:access excludes all three.
 * Showing them as three chips would imply they can be toggled separately.
 */
function groupSources(sources) {
  const byName = new Map();

  for (const s of sources ?? []) {
    const existing = byName.get(s.name);
    if (!existing) {
      byName.set(s.name, {
        name: s.name,
        format: s.format,
        records: s.records,
        files: [s.file],
        assumed: !s.timezone?.startsWith('known'),
        timezones: new Set([s.timezone]),
      });
      continue;
    }

    existing.records += s.records;
    existing.files.push(s.file);
    existing.assumed = existing.assumed || !s.timezone?.startsWith('known');
    existing.timezones.add(s.timezone);
    if (existing.format !== s.format) existing.format = 'mixed';
  }

  return [...byName.values()].map((s) => ({
    ...s,
    title: [
      s.files.join('\n'),
      '',
      `format: ${s.format}`,
      ...[...s.timezones].map((tz) => `timezone: ${tz}`),
      '',
      `click to exclude with -source:${s.name}`,
    ].join('\n'),
  }));
}

function Header({ schema, timeZone }) {
  const formats = new Set((schema?.sources ?? []).map((s) => s.format));

  return (
    <header>
      <span class="brand">loupe</span>
      <span class="path">{schema ? `${schema.sources?.length ?? 0} files` : 'loading…'}</span>
      <span class="meta">
        <b>{formats.size}</b> formats · <b>{number(schema?.records)}</b> records
        {schema?.unparsed > 0 && (
          <>
            {' '}
            · <b>{number(schema.unparsed)}</b> unparsed
          </>
        )}
      </span>
      {/* The active timezone is always on screen, per FILTER-DSL section 2.3. */}
      <span class="tz" title="Times below are shown in this timezone. Bare times you type are read in it too.">
        {timeZone}
      </span>
    </header>
  );
}

function Footer({ result, hist, schema }) {
  const shown = result?.rows?.length ?? 0;
  const total = result?.total ?? 0;

  return (
    <footer>
      <span>
        <b>{number(shown)}</b> of <b>{number(total)}</b> records
        {result?.took_ms !== undefined && <> · <b>{result.took_ms.toFixed(0)}ms</b></>}
      </span>

      {/* Truncation and exclusions are declared, never implied. */}
      {result?.truncated && shown < total && (
        <span class="caution">scroll for more</span>
      )}
      {result?.excluded_no_timestamp > 0 && (
        <span class="caution">
          {number(result.excluded_no_timestamp)} excluded (no timestamp)
        </span>
      )}
      {hist?.no_timestamp > 0 && !result?.excluded_no_timestamp && (
        <span class="caution">{number(hist.no_timestamp)} not on the timeline</span>
      )}

      {result?.window && <span title={result.window.description}>{result.window.description}</span>}

      <span class="right">local · read-only</span>
    </footer>
  );
}

/**
 * What to show when nothing matched.
 *
 * The server explains empty results, so the UI repeats its reasoning rather
 * than inventing its own. A blank table with no explanation is the most
 * misleading thing this tool can display.
 */
function emptyMessage(result, error) {
  if (error) return '';
  if (!result) return 'loading…';
  if (result.explanation?.text) return result.explanation.text;
  return 'No records matched.';
}
