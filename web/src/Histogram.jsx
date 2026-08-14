import { useRef, useState } from 'preact/hooks';
import { clock, dateOnly, rfc3339 } from './format.js';

// Severity order, quietest first, so a stacked bar builds upward to the worst.
const LEVELS = ['trace', 'debug', 'info', 'none', 'warn', 'error', 'fatal'];

const COLOUR = {
  trace: '#2b3742',
  debug: '#2b3742',
  info: '#3d5666',
  none: '#2b3742',
  warn: 'var(--warn)',
  error: 'var(--error)',
  fatal: 'var(--fatal)',
};

const HEIGHT = 50;

/**
 * The timeline.
 *
 * Dragging writes a real DSL string into the filter box rather than setting
 * hidden state. docs/FILTER-DSL.md section 9 calls that the single
 * highest-value detail in the document: the drag teaches the syntax, and the
 * resulting query stays copyable and shareable.
 */
export function Histogram({ hist, timeZone, onRange }) {
  const ref = useRef(null);
  const [drag, setDrag] = useState(null);

  const buckets = hist?.buckets ?? [];
  const max = Math.max(1, hist?.max ?? 1);

  const startDrag = (e) => {
    const rect = ref.current.getBoundingClientRect();
    setDrag({ from: e.clientX - rect.left, to: e.clientX - rect.left });

    const move = (ev) => {
      setDrag((d) => (d ? { ...d, to: ev.clientX - rect.left } : d));
    };

    const up = (ev) => {
      window.removeEventListener('mousemove', move);
      window.removeEventListener('mouseup', up);

      const from = e.clientX - rect.left;
      const to = ev.clientX - rect.left;
      setDrag(null);

      // A click, not a drag: clear the range rather than selecting a sliver.
      if (Math.abs(to - from) < 4) {
        onRange(null);
        return;
      }
      onRange(rangeFor(hist, rect.width, Math.min(from, to), Math.max(from, to)));
    };

    window.addEventListener('mousemove', move);
    window.addEventListener('mouseup', up);
  };

  const label = () => {
    if (!hist?.start) return '';
    const day = dateOnly(hist.start, timeZone);
    return `${day} · ${buckets.length} × ${humanInterval(hist.interval_ms)}`;
  };

  return (
    <div class="histwrap">
      <div class="histhead">
        <span>{hist?.start ? clock(hist.start, timeZone).slice(0, 8) : ''}</span>
        <span class="range">{label()}</span>
        <span>{hist?.end ? clock(hist.end, timeZone).slice(0, 8) : ''}</span>
      </div>

      <div class="hist" ref={ref} onMouseDown={buckets.length ? startDrag : undefined}>
        {buckets.map((b, i) => (
          <div
            class="bar"
            key={i}
            title={`${clock(b.start, timeZone).slice(0, 8)}  ${b.count} record(s)${levelSummary(b)}`}
          >
            {LEVELS.map((level) => {
              const n = b.levels?.[level];
              if (!n) return null;
              return (
                <span
                  key={level}
                  style={{ height: `${segmentHeight(n, max)}px`, background: COLOUR[level] }}
                />
              );
            })}
          </div>
        ))}
      </div>

      {drag && (
        <div
          class="sel"
          style={{
            left: `${Math.min(drag.from, drag.to) + 14}px`,
            width: `${Math.abs(drag.to - drag.from)}px`,
          }}
        />
      )}

      {hist?.no_timestamp > 0 && (
        <div class="hist-note">
          {hist.no_timestamp.toLocaleString('en-GB')} matching record(s) are not on this
          timeline: they have no timestamp. Filter <code>ts:none</code> to see them.
        </div>
      )}
    </div>
  );
}

/**
 * Height of one level's segment within a bar.
 *
 * A bucket that holds records is never drawn as nothing. Against a busy peak a
 * few hundred records round to under a pixel, and a quiet period and a real
 * cluster would then look identical — which is the one thing a timeline exists
 * to distinguish.
 */
function segmentHeight(n, max) {
  return Math.max(2, (n / max) * HEIGHT);
}

/**
 * Turn a pixel span into a DSL time term.
 *
 * Absolute RFC3339 instants rather than a relative expression, so the filter
 * means the same thing tomorrow and on somebody else's machine.
 */
function rangeFor(hist, width, fromPx, toPx) {
  const start = new Date(hist.start).getTime();
  const end = new Date(hist.end).getTime();
  const span = end - start;

  const from = start + Math.max(0, fromPx / width) * span;
  const to = start + Math.min(1, toPx / width) * span;

  return `between:${rfc3339(from)}-${rfc3339(to)}`;
}

function levelSummary(b) {
  const entries = Object.entries(b.levels ?? {});
  if (!entries.length) return '';
  return '\n' + entries.map(([l, n]) => `${l}: ${n}`).join('\n');
}

function humanInterval(ms) {
  if (!ms) return '';
  if (ms >= 86400000) return `${Math.round(ms / 86400000)}d`;
  if (ms >= 3600000) return `${Math.round(ms / 3600000)}h`;
  if (ms >= 60000) return `${Math.round(ms / 60000)}m`;
  if (ms >= 1000) return `${Math.round(ms / 1000)}s`;
  return `${ms}ms`;
}
