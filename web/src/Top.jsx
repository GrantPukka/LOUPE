import { useEffect, useState } from 'preact/hooks';
import { getTop } from './api.js';
import { number, toggleTerm } from './format.js';

/** How many values the panel asks for. The rest are counted, never dropped. */
const TOP_LIMIT = 25;

/**
 * The value breakdown of one field.
 *
 * Answers "which endpoints are 500ing?" in the view you are already looking at.
 * Clicking a value writes a real filter term, so the breakdown is a way into the
 * records rather than a dead end — the same principle as the timeline drag and
 * the pattern rail.
 */
export function Top({ field, filter, timeZone, onClose, onFilter }) {
  const [set, setSet] = useState(null);
  const [error, setError] = useState(null);

  useEffect(() => {
    if (!field) return undefined;

    let live = true;
    setSet(null);
    setError(null);

    getTop({ field, filter, limit: TOP_LIMIT })
      .then((next) => live && setSet(next))
      .catch((e) => live && setError(e.message));

    return () => {
      live = false;
    };
  }, [field, filter]);

  // Escape closes, and only while the panel is open — a listener left attached
  // would swallow the key that clears the filter.
  useEffect(() => {
    if (!field) return undefined;

    const onKey = (e) => {
      if (e.key === 'Escape') {
        e.stopPropagation();
        onClose();
      }
    };
    window.addEventListener('keydown', onKey, true);
    return () => window.removeEventListener('keydown', onKey, true);
  }, [field, onClose]);

  if (!field) return null;

  const largest = Math.max(1, ...(set?.values ?? []).map((v) => v.count));

  return (
    <div class="modal-backdrop" onClick={onClose}>
      <div class="modal top" onClick={(e) => e.stopPropagation()}>
        <div class="modal-head">
          <span>
            Top <b>{field}</b>
            {set && <span class="top-scope"> · {number(set.present)} records</span>}
          </span>
          <button class="clear" onClick={onClose}>
            close
          </button>
        </div>

        {error && <div class="rail-error">{error}</div>}
        {!set && !error && <div class="rail-empty">loading…</div>}

        {set && set.values.length === 0 && (
          <div class="rail-empty">{emptyReason(set)}</div>
        )}

        {set && set.values.length > 0 && (
          <div class="top-values">
            {set.values.map((v) => (
              <button
                class="top-row"
                key={v.value}
                title={`${number(v.count)} records · click to filter on ${field}`}
                onClick={() => {
                  onFilter(toggleTerm(filter, field, v.value));
                  onClose();
                }}
              >
                <span class="top-n">{number(v.count)}</span>
                <span class="top-pct">{(v.share * 100).toFixed(1)}%</span>
                <span class="top-bar">
                  <span class="top-bar-fill" style={{ width: `${(v.count / largest) * 100}%` }} />
                </span>
                <span class="top-v">{displayValue(v.value)}</span>
              </button>
            ))}
          </div>
        )}

        {set && <TopFooter set={set} field={field} onFilter={onFilter} onClose={onClose} />}
      </div>
    </div>
  );
}

/**
 * The denominator, and what sits outside it.
 *
 * A share is meaningless without knowing what it is a share of, and records
 * carrying no value for the field would otherwise vanish from the arithmetic
 * with nothing on screen to say so.
 */
function TopFooter({ set, field, onFilter, onClose }) {
  return (
    <div class="top-foot">
      <div>
        <b>{number(set.distinct)}</b> values across <b>{number(set.present)}</b> records
      </div>

      {set.truncated && (
        <div class="rail-note">
          {number(set.hidden)} more not shown, covering {number(set.hidden_records)} records
        </div>
      )}

      {set.absent > 0 && (
        <div class="rail-note">
          {number(set.absent)} matched records carry no {field}, so they are outside the
          percentages —{' '}
          <button
            class="top-link"
            onClick={() => {
              onFilter(`${field}:none`);
              onClose();
            }}
          >
            show them
          </button>
        </div>
      )}
    </div>
  );
}

function emptyReason(set) {
  if (set.matched === 0) return 'No records matched, so there is nothing to break down.';
  return `None of the ${number(set.matched)} matching records carry a value for ${set.field}.`;
}

/**
 * Render a value so an empty or invisible one is still legible.
 *
 * A field present but empty is a real answer, and a control character renders as
 * nothing at all — both would read as a fault in the tool rather than as the
 * data.
 */
export function displayValue(v) {
  if (v === '') return '(empty)';
  // eslint-disable-next-line no-control-regex
  return String(v).replace(/[\x00-\x1f\x7f]/g, '<ctl>');
}
