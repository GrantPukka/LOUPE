// Formatting helpers shared by the components.

/**
 * Source colours.
 *
 * The mockup hardcoded one per source because it knew them in advance. A real
 * directory has whatever sources it has, so the colour is derived from the name
 * — stable across reloads, and the same source keeps its colour as the filter
 * changes.
 */
const PALETTE = [
  '#5b8cad', '#6f9c8a', '#9a8bb0', '#b08a6f',
  '#7f95a8', '#8a8f99', '#a88f7f', '#7f9ca8',
];

export function sourceColour(name) {
  let hash = 0;
  for (let i = 0; i < name.length; i++) {
    hash = (hash * 31 + name.charCodeAt(i)) >>> 0;
  }
  return PALETTE[hash % PALETTE.length];
}

/**
 * Render an instant in the display timezone.
 *
 * The API sends UTC instants and names the display zone separately, so this is
 * where the two meet. Getting it wrong shows somebody else's clock, which is
 * the failure docs/FILTER-DSL.md section 2.3 exists to prevent.
 */
export function clock(iso, timeZone) {
  if (!iso) return '';

  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';

  const parts = new Intl.DateTimeFormat('en-GB', {
    timeZone,
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
  }).formatToParts(d);

  const get = (type) => parts.find((p) => p.type === type)?.value ?? '00';
  const ms = String(d.getMilliseconds()).padStart(3, '0');

  return `${get('hour')}:${get('minute')}:${get('second')}.${ms}`;
}

export function dateOnly(iso, timeZone) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';

  return new Intl.DateTimeFormat('en-CA', {
    timeZone,
    year: 'numeric',
    month: '2-digit',
    day: '2-digit',
  }).format(d);
}

/** An RFC3339 instant, which is what a DSL time term takes. */
export const rfc3339 = (ms) => new Date(ms).toISOString().replace(/\.\d+Z$/, 'Z');

export const number = (n) => (n ?? 0).toLocaleString('en-GB');

/**
 * Render a value for display without hiding what it is.
 *
 * A null field is shown as an em dash rather than "null", and an object is
 * shown as JSON rather than "[object Object]".
 */
export function displayValue(v) {
  if (v === null || v === undefined) return '—';
  if (typeof v === 'object') return JSON.stringify(v);
  return String(v);
}

/**
 * Quote a value for use in a filter term if it needs it.
 *
 * Clicking a field with a space in it must produce a filter that parses, or
 * click-to-filter silently breaks the query box.
 */
export function quoteValue(v) {
  const s = String(v);
  if (s === '') return '""';
  if (/[\s",:]/.test(s)) {
    return `"${s.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}"`;
  }
  return s;
}

/**
 * Add a term to a filter, replacing an existing term for the same key.
 *
 * Clicking status:200 then status:502 should mean the second, not an
 * impossible conjunction of both.
 */
export function withTerm(filter, key, value) {
  const term = `${key}:${quoteValue(value)}`;
  const kept = (filter || '')
    .split(/\s+/)
    .filter((t) => t && !t.startsWith(`${key}:`) && !t.startsWith(`-${key}:`));

  return [...kept, term].join(' ');
}

/** Drop every time term, so a new range replaces the old one. */
export function withoutTimeTerms(filter) {
  const TIME = /^-?(after|since|before|until|between|last|on):/i;
  return (filter || '')
    .split(/\s+/)
    .filter((t) => t && !TIME.test(t))
    .join(' ');
}

/**
 * Split a filter into its terms, respecting quotes.
 *
 * Splitting on whitespace alone would tear `msg:"read timed out"` into three
 * terms, and removing one of them would leave an unparseable filter.
 */
export function splitTerms(filter) {
  const terms = [];
  let current = '';
  let quoted = false;

  for (let i = 0; i < (filter ?? '').length; i++) {
    const c = filter[i];

    if (c === '\\' && quoted && i + 1 < filter.length) {
      current += c + filter[++i];
      continue;
    }
    if (c === '"') {
      quoted = !quoted;
      current += c;
      continue;
    }
    if (!quoted && /\s/.test(c)) {
      if (current) terms.push(current);
      current = '';
      continue;
    }
    current += c;
  }

  if (current) terms.push(current);
  return terms;
}

/** Remove one term from a filter, leaving the rest intact. */
export function removeTerm(filter, term) {
  return splitTerms(filter)
    .filter((t) => t !== term)
    .join(' ');
}

/** Whether a term is already in a filter. */
export function hasTerm(filter, term) {
  return splitTerms(filter).includes(term);
}

/**
 * Toggle a field term.
 *
 * Clicking a value that is already filtered on removes it. Without this,
 * clicking it again appears to do nothing, which reads as the filter refusing
 * to let go.
 */
export function toggleTerm(filter, key, value) {
  const term = `${key}:${quoteValue(value)}`;
  if (hasTerm(filter, term)) {
    return removeTerm(filter, term);
  }
  return withTerm(filter, key, value);
}

/**
 * A short label for a term chip.
 *
 * A resolved time window is sixty characters of RFC3339, which is correct in
 * the filter box and unreadable as a chip. The full text stays on the chip's
 * title and in the box, which remains the source of truth.
 */
export function termLabel(term) {
  const between = term.match(/^-?between:(\S+?)-(\d{4}-\d{2}-\d{2}T\S+)$/);
  if (between) {
    return `${term.startsWith('-') ? '-' : ''}between: ${clockPart(between[1])} → ${clockPart(between[2])}`;
  }
  if (term.length > 42) return term.slice(0, 41) + '…';
  return term;
}

const clockPart = (iso) => {
  const m = iso.match(/T(\d{2}:\d{2}:\d{2})/);
  return m ? m[1] : iso;
};
