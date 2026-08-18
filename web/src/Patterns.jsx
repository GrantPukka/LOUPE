import { useEffect, useState } from 'preact/hooks';
import { getPatterns } from './api.js';
import { number } from './format.js';

/** How many templates the rail asks for. The rest are counted, never dropped. */
const RAIL_LIMIT = 60;

/**
 * The pattern rail.
 *
 * Thirty-four thousand lines are a dozen shapes with the values filled in
 * differently, and this is where that becomes visible. Clicking a template
 * writes a real `pattern:<id>` term into the filter box — the same principle as
 * the timeline drag: the interaction teaches the syntax and stays shareable.
 */
export function Patterns({ open, filter, applied, onFilter, onClose }) {
  const [set, setSet] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    if (!open) return undefined;

    let live = true;
    setBusy(true);

    getPatterns({ filter: applied, limit: RAIL_LIMIT })
      .then((next) => {
        if (!live) return;
        setSet(next);
        setError(null);
      })
      .catch((e) => {
        if (!live) return;
        setError(e.message);
        setSet(null);
      })
      .finally(() => {
        if (live) setBusy(false);
      });

    // A filter can change while a listing is in flight, and the stale answer
    // must not land on top of the fresh one.
    return () => {
      live = false;
    };
  }, [open, applied]);

  if (!open) return null;

  // Which template the filter is currently pinned to, so the rail can show it
  // as selected and offer to unpin it.
  const active = (filter.match(/(?:^|\s)pattern:([0-9a-fA-F]+)/) ?? [])[1] ?? null;

  return (
    <aside class="rail">
      <div class="rail-head">
        <span>patterns</span>
        {busy && <span class="rail-busy">…</span>}
        <button class="rail-x" onClick={onClose} title="hide the pattern list">
          ×
        </button>
      </div>

      {error && <div class="rail-error">{error}</div>}

      <div class="rail-list">
        {set?.patterns?.map((p) => (
          <button
            class={`rail-item ${active && p.id.startsWith(active) ? 'on' : ''}`}
            key={p.id}
            title={railTitle(p)}
            onClick={() => onFilter(togglePattern(filter, p.id, active))}
          >
            <span class="rail-n">{number(p.count)}</span>
            <span class="rail-t">{p.template}</span>
          </button>
        ))}

        {set && set.patterns.length === 0 && !error && (
          <div class="rail-empty">No templates match this filter.</div>
        )}
      </div>

      {set && <RailFooter set={set} />}
    </aside>
  );
}

/**
 * What the rail is not showing.
 *
 * A list that stops without saying so understates the data, and the tail here
 * is not noise: a burst of one-off templates usually means a burst of broken
 * lines, which is itself worth knowing.
 */
function RailFooter({ set }) {
  return (
    <div class="rail-foot">
      <div>
        <b>{number(set.templates)}</b> templates · <b>{number(set.records)}</b> records
      </div>

      {set.truncated && (
        <div class="rail-note">
          {number(set.hidden)} more not shown, covering {number(set.hidden_records)} records
        </div>
      )}

      {set.unparsed_templates > 0 && (
        <div class="rail-note">
          {number(set.unparsed_templates)} from {number(set.unparsed_records)} unreadable lines —
          a broken line is its own shape
        </div>
      )}
    </div>
  );
}

function railTitle(p) {
  const lines = [p.template, ''];

  if (p.example && p.example !== p.template) {
    lines.push(`example: ${p.example}`, '');
  }
  if (p.sources?.length) {
    lines.push(`sources: ${p.sources.join(', ')}`);
  }
  if (p.no_timestamp) {
    lines.push(`${p.no_timestamp} of these have no timestamp`);
  }

  lines.push('', `click to filter with pattern:${p.id}`);
  return lines.join('\n');
}

/**
 * Add, swap, or remove the pattern term.
 *
 * Clicking the selected template clears it, so the rail is a toggle rather
 * than a one-way trip that leaves the user hunting for the × in the filter.
 */
export function togglePattern(filter, id, active) {
  const without = (filter ?? '')
    .split(/\s+/)
    .filter((term) => term && !/^-?pattern:/i.test(term))
    .join(' ');

  if (active && id.startsWith(active)) return without;
  return `${without} pattern:${id}`.trim();
}
