import { useCallback, useEffect, useRef, useState } from 'preact/hooks';
import { LIST_COLUMNS, getHistogram, getSchema, getSubscriptions, openTail, runQuery } from './api.js';
import { Browser, Subscriptions } from './Browser.jsx';
import { FilterHelp } from './FilterHelp.jsx';
import { Histogram } from './Histogram.jsx';
import { alignRows, prependNewest, withoutDuplicates } from './live.js';
import { Rows } from './Rows.jsx';
import { number, removeTerm, sourceColour, splitTerms, termLabel, withoutTimeTerms } from './format.js';

// One page of records. Scrolling fetches the next.
const PAGE = 300;

// Typing pause before a query runs. Long enough not to query on every
// keystroke, short enough that the list feels attached to the box.
const DEBOUNCE_MS = 220;

// How often the timeline is refreshed while records are streaming in.
//
// Every arrival cannot redraw it: during an incident that is several redraws a
// second, and the histogram is a whole-dataset query. Two seconds is slow
// enough to be cheap and fast enough that the bar you are watching grows while
// you watch it.
const HISTOGRAM_REFRESH_MS = 2000;

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

  // Live tail. Off until asked for: opening the page must not start polling
  // somebody's log directory, and someone reading a window from last Tuesday
  // should not be dragged to the present.
  const [live, setLive] = useState(false);
  // Records that arrived while the list was scrolled away from the top. Held
  // rather than inserted, because moving the rows under a reader mid-incident
  // is the fastest way to lose their place. The rows live in a ref and only
  // their count is state: rendering needs the number, not the records.
  const pending = useRef([]);
  const [pendingCount, setPendingCount] = useState(0);
  // Bumped to ask the list to jump to the top. A counter rather than a
  // callback into Rows, so the request survives a re-render and stays one-way.
  const [jumpToTop, setJumpToTop] = useState(0);
  const [notices, setNotices] = useState([]);
  const atTop = useRef(true);

  // The latest result, readable without waiting for a re-render. A streamed
  // batch has to merge against what is on screen right now, and two batches
  // can land before Preact has committed the first.
  const resultRef = useRef(null);
  // How many live records have arrived, and how many the timeline has seen.
  // Equal means nothing changed and the histogram query can be skipped.
  const arrived = useRef(0);
  const drawn = useRef(0);

  const input = useRef(null);
  const generation = useRef(0);
  // Set when the next filter change came from a click rather than the keyboard,
  // so it applies without waiting out the typing debounce.
  const immediate = useRef(false);

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
  //
  // This effect is the only writer of `applied`. Setting it from the click
  // handlers as well let the two drift apart, leaving the box showing one
  // filter and the list showing another.
  useEffect(() => {
    const delay = immediate.current ? 0 : DEBOUNCE_MS;
    immediate.current = false;

    const timer = setTimeout(() => setApplied((filter ?? '').trim()), delay);
    return () => clearTimeout(timer);
  }, [filter]);

  /**
   * Change the filter and query at once, skipping the debounce.
   *
   * A click is a decision, not typing. Waiting a fifth of a second after one
   * reads as the filter refusing to let go.
   */
  const applyNow = useCallback((next) => {
    immediate.current = true;
    setFilter(next);
  }, []);

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
    const loaded = result?.rows?.length ?? 0;
    if (!result || busy || loaded >= result.total) return;

    const mine = generation.current;
    setBusy(true);
    try {
      const next = await runQuery({
        filter: applied,
        limit: PAGE,
        offset: loaded,
        columns: LIST_COLUMNS,
        sort,
      });
      if (mine !== generation.current) return;

      setResult((prev) =>
        prev ? { ...prev, rows: [...(prev.rows ?? []), ...(next.rows ?? [])] } : next,
      );
    } catch {
      // Leave what is already loaded on screen.
    } finally {
      if (mine === generation.current) setBusy(false);
    }
  }, [applied, busy, result, sort]);

  useEffect(() => {
    resultRef.current = result;
  }, [result]);

  /**
   * Fold a streamed batch into the list, or hold it if the reader has moved.
   *
   * Held records still count towards the total. The footer saying "412 of
   * 33,000" while 12 more sit unshown would be a quiet lie about how much data
   * there is.
   */
  const onLive = useCallback((payload) => {
    const prev = resultRef.current;
    if (!prev) return;

    const seqAt = (prev.columns ?? []).indexOf('seq');
    const rows = alignRows(payload.rows, payload.columns, prev.columns);
    const fresh = withoutDuplicates(rows, seqAt, prev.rows, pending.current);
    if (!fresh.length) return;

    arrived.current += fresh.length;
    const total = (prev.total ?? 0) + fresh.length;

    if (!atTop.current) {
      pending.current = [...pending.current, ...fresh];
      setPendingCount(pending.current.length);
      resultRef.current = { ...prev, total };
      setResult(resultRef.current);
      return;
    }

    resultRef.current = { ...prev, rows: prependNewest(prev.rows ?? [], fresh), total };
    setResult(resultRef.current);
  }, []);

  /**
   * Show what was held back.
   *
   * The list jumps to the top as well. "Click to show" that leaves the reader
   * where they were, looking at the same rows, has not shown them anything.
   */
  const flushPending = useCallback(() => {
    const held = pending.current;
    pending.current = [];
    setPendingCount(0);
    if (!held.length) return;

    setResult((prev) => (prev ? { ...prev, rows: prependNewest(prev.rows ?? [], held) } : prev));
    setJumpToTop((n) => n + 1);
  }, []);

  /**
   * Pause while the reader is not at the top, resume when they return.
   *
   * Without this the list shuffles under the cursor every time a record
   * arrives, which during an incident is constantly.
   */
  const onAtTop = useCallback((value) => {
    atTop.current = value;
    if (value) flushPending();
  }, [flushPending]);

  const note = useCallback((message) => {
    // Repeating the same warning on every poll would bury the screen in it.
    setNotices((held) => (held.includes(message) ? held : [...held, message]));
  }, []);

  // The live stream. Reopened when the filter changes, so what streams in and
  // what the table holds are always the answer to the same question.
  useEffect(() => {
    if (!live) {
      pending.current = [];
      setPendingCount(0);
      setNotices([]);
      return undefined;
    }
    return openTail({ filter: applied, onRecords: onLive, onNotice: note, onError: note });
  }, [live, applied, onLive, note]);

  // Redraw the timeline while records are arriving, so the bar being watched
  // grows. Skipped when nothing came in, because the histogram is a query over
  // the whole dataset and polling it for no reason is not free.
  useEffect(() => {
    if (!live) return undefined;

    const timer = setInterval(() => {
      if (arrived.current === drawn.current) return;
      drawn.current = arrived.current;
      getHistogram({ filter: applied, buckets: 90 }).then(setHist).catch(() => {
        // The rows are still streaming; a stale timeline is better than an
        // error bar over a working tail.
      });
    }, HISTOGRAM_REFRESH_MS);

    return () => clearInterval(timer);
  }, [live, applied]);

  /**
   * Start or stop following.
   *
   * Starting forces newest-first: a live tail is about what just happened, and
   * appending arrivals to the bottom of an oldest-first list puts them off the
   * end of a page nobody is looking at.
   */
  const toggleLive = useCallback(() => {
    setLive((on) => {
      if (!on) setSort('-time');
      return !on;
    });
  }, []);

  /**
   * Clearing the filter returns to the newest records.
   *
   * Someone who has narrowed to a window and then clears it wants to be back at
   * "what is happening now", not left looking at wherever the old range put
   * them. Forcing newest-first and scrolling to the top does that.
   */
  const clearFilter = useCallback(() => {
    immediate.current = true;
    setFilter('');
    // Not routed through the debounce: if the box is already empty setFilter
    // is a no-op, the effect never re-runs, and a stale `applied` would keep
    // the old results on screen with nothing left to explain them.
    setApplied('');
    setSort('-time');
  }, []);

  /** Remove one term, leaving the rest of the filter alone. */
  const dropTerm = useCallback((term) => {
    immediate.current = true;
    setFilter((current) => removeTerm(current, term));
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
    immediate.current = true;
    setFilter((current) => {
      const base = withoutTimeTerms(current);
      const next = term ? `${base} ${term}`.trim() : base;
      return next;
    });
  }, []);

  const toggleSource = useCallback((name) => {
    immediate.current = true;
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
    ((filter ?? '').match(/-source:(\S+)/g) ?? []).map((t) => t.slice('-source:'.length)),
  );

  // Read once, defensively.
  //
  // A render that throws stops Preact updating anything, so the page freezes
  // with the old filter still on screen and every control dead. That is a much
  // worse failure than a missing row count, and it is not worth risking on an
  // assumption about a field's shape.
  const rows = result?.rows ?? [];

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
        <span class="busy">{busy ? '…' : ''}</span>
        {filter && (
          <button class="clear" onClick={clearFilter} title="Escape — returns to the newest records">
            clear
          </button>
        )}
        <button
          class={`clear live ${live ? 'on' : ''}`}
          onClick={toggleLive}
          title={
            live
              ? 'stop following — the table stops updating on its own'
              : 'follow: show records as they are written to the log files'
          }
        >
          {live ? '● live' : '○ live'}
        </button>
        <button
          class={`clear ${showHelp ? 'on' : ''}`}
          onClick={() => setShowHelp((v) => !v)}
          title="filter syntax (?)"
        >
          ? syntax
        </button>
      </div>

      {/* Above the chips, not below them.
          The chip row appears and disappears as terms come and go, and anything
          under it moves when it does. With the panel below, clicking an example
          shifted the panel — and its close button — out from under the cursor
          mid-click. */}
      <FilterHelp
        open={showHelp}
        timezone={timeZone}
        onClose={() => setShowHelp(false)}
        onInsert={(term) => applyNow(`${filter ?? ''} ${term}`.trim())}
      />

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

      {error && <div class="error-bar">{error}</div>}

      {/* A source that stopped being readable, or a stream that fell behind.
          Never folded into silence: a live tail that has quietly stopped
          covering one file is exactly the confident wrong answer this tool
          exists to avoid. */}
      {notices.map((message) => (
        <div class="notice-bar" key={message}>
          {message}
          <button class="clear" onClick={() => setNotices((held) => held.filter((m) => m !== message))}>
            dismiss
          </button>
        </div>
      ))}

      <Histogram hist={hist} timeZone={timeZone} onRange={onRange} />

      <div class="colhead">
        <span class="c-ts">time</span>
        <span class="c-lvl">level</span>
        <span class="c-src">source</span>
        <span class="c-msg">message</span>
      </div>

      {pendingCount > 0 && (
        <button class="pending" onClick={flushPending}>
          {number(pendingCount)} new {pendingCount === 1 ? 'record' : 'records'} — click to show
        </button>
      )}

      <Rows
        rows={rows}
        columns={result?.columns ?? []}
        timeZone={timeZone}
        filter={filter}
        onFilter={applyNow}
        onLoadMore={loadMore}
        onAtTop={onAtTop}
        jumpToTop={jumpToTop}
        hasMore={!!result && rows.length < result.total}
        empty={emptyMessage(result, error)}
      />

      <Footer result={result} hist={hist} schema={schema} sort={sort} onSort={setSort} live={live} />

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

function Footer({ result, hist, schema, sort, onSort, live }) {
  const shown = result?.rows?.length ?? 0;
  const total = result?.total ?? 0;

  return (
    <footer>
      {/* Following pins the order. Arrivals go on top; oldest-first would put
          them off the end of a list nobody is looking at, which reads as the
          tail having stopped. */}
      <button
        class="clear"
        disabled={live}
        onClick={() => onSort(sort === '-time' ? 'time' : '-time')}
        title={
          live
            ? 'newest first while following — stop the live tail to sort oldest first'
            : 'switch between newest and oldest first'
        }
      >
        {sort === '-time' ? 'newest first' : 'oldest first'}
      </button>
      {live && <span class="live-mark">● following</span>}
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
