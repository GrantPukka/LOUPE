import { useEffect, useRef, useState } from 'preact/hooks';
import { getRecord } from './api.js';
import { clock, displayValue, hasTerm, quoteValue, sourceColour, toggleTerm } from './format.js';

const ROW_HEIGHT = 22;
// Rows rendered beyond the visible window, so a fast scroll does not show gaps.
const OVERSCAN = 12;

/**
 * The record list.
 *
 * Virtualised: only the visible window is in the DOM. A filter matching 30,000
 * records would otherwise build 30,000 nodes and lock the tab, and the whole
 * promise of the tool is that it stays responsive on a real log directory.
 */
export function Rows({
  rows, columns, timeZone, filter, onFilter, onLoadMore, onAtTop, jumpToTop, hasMore, empty,
  onTop,
}) {
  const scroller = useRef(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [height, setHeight] = useState(600);
  const [open, setOpen] = useState(null);
  const [detail, setDetail] = useState(null);

  useEffect(() => {
    const el = scroller.current;
    if (!el) return;

    const measure = () => setHeight(el.clientHeight);
    measure();

    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, []);

  // A new result set starts at the top with nothing expanded; leaving a row
  // open would attach a detail panel to a different record.
  useEffect(() => {
    setOpen(null);
    setDetail(null);
    setScrollTop(0);
    if (scroller.current) scroller.current.scrollTop = 0;
  }, [filter]);

  // Whether the reader is at the top, which is what decides if a live tail may
  // insert rows or has to hold them. A row's worth of slack, so nudging the
  // wheel does not count as having scrolled away.
  useEffect(() => {
    onAtTop?.(scrollTop < ROW_HEIGHT);
  }, [scrollTop, onAtTop]);

  // Held live records were just released, so go and look at them. Skipped on
  // the first render: the initial value is not a request.
  useEffect(() => {
    if (!jumpToTop) return;
    setScrollTop(0);
    if (scroller.current) scroller.current.scrollTop = 0;
  }, [jumpToTop]);

  const index = columnIndex(columns);

  const onScroll = (e) => {
    setScrollTop(e.target.scrollTop);

    // Fetch the next page before the user reaches the end, so scrolling does
    // not stall at the boundary.
    const { scrollHeight, clientHeight, scrollTop: top } = e.target;
    if (hasMore && scrollHeight - top - clientHeight < clientHeight) {
      onLoadMore();
    }
  };

  const toggle = async (seq) => {
    // Without a seq there is nothing to fetch and no way to tell rows apart,
    // so expanding is a no-op rather than opening every row at once.
    if (seq === undefined || seq === null) return;

    if (open === seq) {
      setOpen(null);
      setDetail(null);
      return;
    }

    setOpen(seq);
    setDetail(null);
    try {
      const res = await getRecord(seq);
      if (res.rows?.length) setDetail(recordOf(res));
    } catch {
      // A failed detail fetch leaves the row expanded but empty rather than
      // breaking the list; the record itself is still on screen.
      setDetail(null);
    }
  };

  if (!rows.length) {
    return (
      <div class="rows" ref={scroller}>
        <div class="empty">{empty}</div>
      </div>
    );
  }

  const first = Math.max(0, Math.floor(scrollTop / ROW_HEIGHT) - OVERSCAN);
  const visible = Math.ceil(height / ROW_HEIGHT) + OVERSCAN * 2;
  const last = Math.min(rows.length, first + visible);

  const window = [];
  let previous = '';

  // The leading digits a row shares with the one above are dimmed, so the eye
  // lands on what changed. That needs the row before the window too.
  if (first > 0) previous = clock(rows[first - 1][index.ts], timeZone);

  for (let i = first; i < last; i++) {
    const row = rows[i];
    const seq = row[index.seq];
    const stamp = clock(row[index.ts], timeZone);

    let shared = 0;
    while (shared < stamp.length && stamp[shared] === previous[shared]) shared++;
    previous = stamp;

    window.push(
      <Row
        key={seq ?? i}
        row={row}
        index={index}
        stamp={stamp}
        shared={shared}
        expanded={open === seq}
        onClick={() => toggle(seq)}
      />,
    );

    if (open === seq) {
      window.push(
        <Detail
          key={`d-${seq}`}
          record={detail}
          filter={filter}
          onFilter={onFilter}
          onTop={onTop}
        />,
      );
    }
  }

  return (
    <div class="rows" ref={scroller} onScroll={onScroll}>
      <div style={{ height: `${first * ROW_HEIGHT}px` }} />
      {window}
      <div style={{ height: `${Math.max(0, (rows.length - last) * ROW_HEIGHT)}px` }} />
      {hasMore && (
        <div class="empty" style="padding:14px">
          loading more…
        </div>
      )}
    </div>
  );
}

function Row({ row, index, stamp, shared, expanded, onClick }) {
  const level = row[index.level] ?? '';
  const source = row[index.source] ?? '';

  return (
    <div
      class={`row ${level}`}
      onClick={onClick}
      style={{ background: expanded ? 'var(--panel)' : undefined }}
    >
      <span class="c-ts">
        {stamp ? (
          <>
            {stamp.slice(0, shared)}
            <b>{stamp.slice(shared)}</b>
          </>
        ) : (
          <b>no timestamp</b>
        )}
      </span>
      <span class={`c-lvl lvl-${level}`}>{level}</span>
      <span class="c-src" style={{ '--src': sourceColour(source) }}>
        <i>{source}</i>
      </span>
      <span class="c-msg">
        {firstLine(row[index.message])}
        {extraLines(row[index.message]) > 0 && (
          <span class="multi">+{extraLines(row[index.message])} lines</span>
        )}
      </span>
    </div>
  );
}

/**
 * The expanded record.
 *
 * Clicking a field value adds it to the filter. ARCHITECTURE section 6 calls
 * that interaction most of the perceived magic, and it works here because the
 * value goes through the same DSL the user could have typed.
 *
 * The raw line is always shown, in its native format. The receiver of a finding
 * may not trust our parser, and they are right not to.
 */
function Detail({ record, filter, onFilter, onTop }) {
  if (!record) {
    return (
      <div class="detail">
        <span style="color:var(--text-ghost)">loading…</span>
      </div>
    );
  }

  const rows = [
    ['source', record.source],
    ['file', record.file],
    ['format', record.format],
    ['line', record.line_no],
    ...(record.ts ? [['ts', record.ts]] : []),
    ...Object.entries(record.fields ?? {}),
  ];

  return (
    <div class="detail">
      {record.parsed === false && (
        <div class="kv">
          <span class="k">parsed</span>
          <span style="color:var(--warn)">
            no — this line matched no parser, so only its raw text is available
          </span>
        </div>
      )}

      {record.ts && record.ts_zoned === false && (
        <div class="kv">
          <span class="k">timezone</span>
          <span style="color:var(--warn)">
            assumed — this format carries no offset, so the time depends on --source-tz
          </span>
        </div>
      )}

      {rows.map(([k, v]) => {
        const applied = hasTerm(filter, `${k}:${quoteValue(v)}`);
        return (
          <div class="kv" key={k}>
            <span class="k">{k}</span>
            <span
              class={`v ${applied ? 'on' : ''}`}
              title={applied ? `remove ${k} from the filter` : `filter on ${k}`}
              onClick={(e) => {
                e.stopPropagation();
                onFilter(toggleTerm(filter, k, v));
              }}
            >
              {displayValue(v)}
              {applied && <span class="v-on"> ✓ filtering</span>}
            </span>

            {/* The breakdown of this field, not of this value: the useful
                question is "which paths are there", asked from a record that
                happens to have one. */}
            <button
              class="v-top"
              title={`count every value of ${k}`}
              onClick={(e) => {
                e.stopPropagation();
                onTop?.(k);
              }}
            >
              % top
            </button>
          </div>
        );
      })}

      <div class="rawhead">
        raw line · {record.format} · {record.file}
      </div>
      <div class="raw">{record.raw}</div>
    </div>
  );
}

/** Map column names to positions, so the row arrays can be read by name. */
function columnIndex(columns) {
  const index = {};
  (columns ?? []).forEach((name, i) => {
    index[name] = i;
  });
  return index;
}

/** Rebuild one record object from a detail query's single row. */
function recordOf(res) {
  const record = {};
  res.columns.forEach((name, i) => {
    record[name] = res.rows[0][i];
  });

  if (typeof record.fields === 'string') {
    try {
      record.fields = JSON.parse(record.fields);
    } catch {
      // Leave it as text: an undecodable bag is still worth showing.
    }
  }
  return record;
}

const firstLine = (s) => (s ?? '').split('\n')[0];
const extraLines = (s) => Math.max(0, (s ?? '').split('\n').length - 1);
