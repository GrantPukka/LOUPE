import { useEffect, useState } from 'preact/hooks';
import { getTrace } from './api.js';
import { clock, sourceColour } from './format.js';

/**
 * One request's path across every source.
 *
 * The waits between hops are the finding — a trace is usually five lines that
 * all look fine and one four-second gap between two of them — so the gap is
 * what the row is built around, and the longest one is drawn as a bar.
 */
export function Trace({ id, field, timeZone, onClose, onFilter }) {
  const [trace, setTrace] = useState(null);
  const [error, setError] = useState(null);

  useEffect(() => {
    if (!id) return undefined;

    let live = true;
    setTrace(null);
    setError(null);

    getTrace({ id, field })
      .then((next) => live && setTrace(next))
      .catch((e) => live && setError(e.message));

    return () => {
      live = false;
    };
  }, [id, field]);

  // Escape closes, like every other panel.
  //
  // Only while a trace is open. The listener captures, so leaving it attached
  // when the panel is closed swallowed Escape before the rest of the app saw
  // it — and Escape is also how the filter is cleared and the help panel is
  // dismissed.
  useEffect(() => {
    if (!id) return undefined;

    const onKey = (e) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        onClose();
      }
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [id, onClose]);

  if (!id) return null;

  const longest = Math.max(1, ...(trace?.hops ?? []).map((h) => h.gap ?? 0));

  return (
    <div class="modal-backdrop" onClick={onClose}>
      <div class="modal trace" onClick={(e) => e.stopPropagation()}>
        <div class="modal-head">
          <span>
            Trace <b>{id}</b>
            {trace?.field && <span class="trace-field"> · {trace.field}</span>}
          </span>
          <button class="clear" onClick={onClose}>
            close
          </button>
        </div>

        {error && <div class="rail-error">{error}</div>}
        {!trace && !error && <div class="rail-empty">loading…</div>}

        {trace && trace.hops.length === 0 && (
          <div class="rail-empty">No records carry {trace.field} {id}.</div>
        )}

        {trace && trace.hops.length > 0 && (
          <div class="trace-hops">
            {trace.hops.map((h, i) => (
              <Hop
                key={`${h.seq}`}
                hop={h}
                longest={longest}
                slowest={i === trace.slowest}
                timeZone={timeZone}
              />
            ))}
          </div>
        )}

        {trace && <TraceFooter trace={trace} onFilter={onFilter} onClose={onClose} />}
      </div>
    </div>
  );
}

function Hop({ hop, longest, slowest, timeZone }) {
  // The bar is the wait *before* this hop, scaled against the longest wait in
  // the trace. Absolute durations would make a 4ms trace and a 4s one look
  // identical, which is the opposite of the point.
  const width = hop.gap ? Math.max(2, Math.round((hop.gap / longest) * 100)) : 0;

  return (
    <div class={`hop ${slowest ? 'slow' : ''}`}>
      <span class="hop-ts">
        {hop.time ? clock(hop.time, timeZone) : <i class="hop-undated">no timestamp</i>}
      </span>

      <span class="hop-gap">{hop.has_gap ? `+${humanGap(hop.gap)}` : ''}</span>

      <span class="hop-bar">
        {width > 0 && <span class="hop-bar-fill" style={{ width: `${width}%` }} />}
      </span>

      <span class="hop-src" style={{ '--src': sourceColour(hop.source) }}>
        {hop.source}
      </span>
      <span class={`hop-lvl lvl-${hop.level}`}>{hop.level}</span>
      <span class="hop-msg" title={hop.message}>
        {firstLine(hop.message)}
      </span>
    </div>
  );
}

/**
 * What the trace cannot tell you.
 *
 * A trace is a claim about where a request went, and the sources that could
 * never have answered are invisible unless named. Listing only the services
 * that matched invites the reader to conclude the request skipped the rest.
 */
function TraceFooter({ trace, onFilter, onClose }) {
  const blind = (trace.blind ?? []).map((r) => r.name);
  const silent = (trace.silent ?? []).map((r) => r.name);

  return (
    <div class="trace-foot">
      {trace.span > 0 && (
        <div>
          Span <b>{humanGap(trace.span)}</b>
          {trace.slowest >= 0 && trace.hops[trace.slowest]?.has_gap && (
            <>
              , of which <b>{humanGap(trace.hops[trace.slowest].gap)}</b> waiting before{' '}
              {trace.hops[trace.slowest].source}
            </>
          )}
        </div>
      )}

      {blind.length > 0 && (
        <div class="rail-note">
          {blind.join(', ')} never record {trace.field}, so this trace cannot say whether the
          request reached them.
        </div>
      )}
      {silent.length > 0 && (
        <div class="trace-quiet">
          {silent.join(', ')} record {trace.field} but none for this one.
        </div>
      )}
      {trace.undated > 0 && (
        <div class="rail-note">
          {trace.undated} hop{trace.undated === 1 ? '' : 's'} carry no timestamp and are listed
          last, in ingest order.
        </div>
      )}

      {trace.hops.length > 0 && (
        <button
          class="clear"
          onClick={() => {
            onFilter(`${trace.field}:${trace.id}`);
            onClose();
          }}
        >
          show these records in the table
        </button>
      )}
    </div>
  );
}

/**
 * Nanoseconds to something comparable at a glance.
 *
 * Go encodes a duration as nanoseconds, and full precision on a four-
 * millisecond wait next to a four-second one makes the two hard to tell apart.
 */
export function humanGap(ns) {
  const ms = ns / 1e6;

  if (ms >= 60_000) return `${(ms / 60_000).toFixed(1)}m`;
  if (ms >= 1000) return `${(ms / 1000).toFixed(2)}s`;
  if (ms >= 1) return `${Math.round(ms)}ms`;
  return `${Math.round(ns / 1000)}µs`;
}

const firstLine = (s) => (s ?? '').split('\n')[0];
