import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { LIST_COLUMNS, getHistogram, getSchema, getSubscriptions, runQuery } from './api.js';
import { Browser, Subscriptions } from './Browser.jsx';
import { FilterHelp } from './FilterHelp.jsx';
import { Histogram } from './Histogram.jsx';
import { Rows } from './Rows.jsx';
import { number, removeTerm, sourceColour, splitTerms, termLabel, withoutTimeTerms } from './format.js';

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

  const [showHelp, setShowHelp] = useState(false);
  const [showBrowser, setShowBrowser] = useState(false);
  const [showSubs, setShowSubs] = useState(false);
  const [subs, setSubs] = useState(null);

  // Newest first. The top of the list is where the eye starts, and the most
  // recent record is almost always the one being looked for.
  const [sort, setSort] = useState('-time');

  const input = useRef(null);
  const generation = useRef(0);

  useEffect(() => {
    getSchema().then(setSchema).catch((e) => setError(e.message));
    getSubscriptions().then(setSubs).catch(() => {
      // An older binary, or one started without a workspace. The rest of the
      // UI works; only the subscription controls are unavailable.
      setSubs(null);
    });
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
        runQuery({ filter: expression, limit: PAGE, columns: LIST_COLUMNS, sort }),
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
  }, [sort]);

  useEffect(() => {
    load(applied);
  }, [applied, sort, load]);

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
        sort,
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
  }, [applied, busy, result, sort]);

  /**
   * Clearing the filter returns to the newest records.
   *
   * Someone who has narrowed to a window and then clears it wants to be back at
   * "what is happening now", not left looking at wherever the old range put
   * them. Forcing newest-first and scrolling to the top does that.
   */
  const clearFilter = useCallback(() => {
    setFilter('');
    setApplied('');
    setSort('-time');
  }, []);

  /**
   * Remove one term.
   *
   * Applied immediately rather than through the debounce: a click is a
   * decision, not typing, and waiting a fifth of a second after it reads as
   * the filter refusing to let go.
   */
  const dropTerm = useCallback((term) => {
    setFilter((current) => {
      const next = removeTerm(current, term);
      setApplied(next.trim());
      return next;
    });
  }, []);

  // Keyboard: / focuses the filter, Escape clears it.
  useEffect(() => {
    const onKey = (e) => {
      if (e.key === '/' && document.activeElement !== input.current) {
        e.preventDefault();
        input.current?.focus();
      }
      if (e.key === 'Escape') {
        if (showBrowser || showSubs || showHelp) {
          setShowBrowser(false);
          setShowSubs(false);
          setShowHelp(false);
          return;
        }
        // From anywhere, not only the box. Someone scrolling the list who
        // wants out of a filter should not have to click into the input
        // first.
        clearFilter();
        input.current?.blur();
      }
      if (e.key === '?' && document.activeElement !== input.current) {
        e.preventDefault();
        setShowHelp((v) => !v);
      }
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [showBrowser, showSubs, showHelp, clearFilter]);

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
        {subs && (
          <>
            <button class="chip add" onClick={() => setShowBrowser(true)} title="add a log location">
              + add folder
            </button>
            <button class="chip add" onClick={() => setShowSubs(true)} title="manage subscriptions">
              {(subs.subscriptions ?? []).filter((s) => s.active).length} subscribed
            </button>
          </>
        )}

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
          <button class="clear" onClick={clearFilter} title="Escape — returns to the newest records">
            clear
          </button>
        )}
        <button
          class={`clear ${showHelp ? 'on' : ''}`}
          onClick={() => setShowHelp((v) => !v)}
          title="filter syntax (?)"
        >
          ? syntax
        </button>
      </div>

      {applied && (
        <div class="terms">
          <span class="terms-label">filtering on</span>
          {splitTerms(applied).map((term) => (
            <button
              class="term"
              key={term}
              title={`${term}\n\nclick to remove this term`}
              onClick={() => dropTerm(term)}
            >
              {termLabel(term)}
              <span class="term-x">×</span>
            </button>
          ))}
          <button class="term-all" onClick={clearFilter} title="remove every term (Escape)">
            clear all
          </button>
        </div>
      )}

      <FilterHelp
        open={showHelp}
        timezone={timeZone}
        onClose={() => setShowHelp(false)}
        onInsert={(term) => setFilter((f) => `${f} ${term}`.trim())}
      />

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

      <Footer result={result} hist={hist} schema={schema} sort={sort} onSort={setSort} />

      <Browser
        open={showBrowser}
        onClose={() => setShowBrowser(false)}
        onChanged={() => getSubscriptions().then(setSubs).catch(() => {})}
      />
      <Subscriptions
        open={showSubs}
        subs={subs}
        onClose={() => setShowSubs(false)}
        onChanged={() => getSubscriptions().then(setSubs).catch(() => {})}
      />
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

function Footer({ result, hist, schema, sort, onSort }) {
  const shown = result?.rows?.length ?? 0;
  const total = result?.total ?? 0;

  return (
    <footer>
      <button
        class="clear"
        onClick={() => onSort(sort === '-time' ? 'time' : '-time')}
        title="switch between newest and oldest first"
      >
        {sort === '-time' ? 'newest first' : 'oldest first'}
      </button>
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
