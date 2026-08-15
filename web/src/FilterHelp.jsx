/**
 * The filter cheatsheet.
 *
 * The DSL is small but nobody remembers a syntax they use twice a month, and
 * the alternative to a panel like this is a user typing something plausible,
 * getting an error, and giving up. Every example is clickable, so the fastest
 * way to learn the syntax is to use it.
 */

const SECTIONS = [
  {
    title: 'severity',
    items: [
      ['level:error', 'one level'],
      ['level:>=warn', 'warn and above'],
      ['level:error,fatal', 'either'],
      ['-level:debug', 'everything except debug'],
    ],
  },
  {
    title: 'time',
    items: [
      ['last:15m', 'relative to the newest record, not to now'],
      ['14:00-15:00', 'a window, in the timezone shown above'],
      ['after:14:00', 'from then on'],
      ['on:2026-08-13', 'that whole day'],
      ['ts:none', 'records with no timestamp at all'],
    ],
  },
  {
    title: 'where it came from',
    items: [
      ['source:nginx', 'one source — a prefix will do'],
      ['-source:nginx', 'everything else'],
      ['file:access.log*', 'a file, glob included'],
      ['format:jsonl', 'everything one parser read'],
    ],
  },
  {
    title: 'any field',
    items: [
      ['status:>=500', 'numeric comparison'],
      ['latency_ms:>1000', 'the same, on any field'],
      ['trace_id:a91c40f2', 'one request across services'],
      ['user_id:*', 'the field is present'],
      ['region:none', 'the field is absent'],
    ],
  },
  {
    title: 'text',
    items: [
      ['timeout', 'a bare word searches the message and every field'],
      ['"read timed out"', 'an exact phrase'],
      ['message~timeout', 'the message only'],
      ['message~/^GET \\/api/', 'a regex'],
    ],
  },
];

export function FilterHelp({ open, onClose, onInsert, timezone }) {
  if (!open) return null;

  return (
    <div class="help">
      <div class="help-head">
        <span>
          Terms are joined by <b>and</b>. Commas within one term mean <b>or</b>. Click an
          example to add it.
        </span>
        <button class="clear" onClick={onClose}>
          close
        </button>
      </div>

      {/* How to build one, not just what the pieces are. The syntax table
          below answers "what can I type"; this answers "how do I work". */}
      <div class="help-how">
        <div>
          <b>Build one</b> by stacking terms: <code>level:&gt;=error</code>{' '}
          <code>source:nginx</code> <code>status:&gt;=500</code> means all three at once.
        </div>
        <div>
          <b>Narrow by time</b> by dragging the timeline — it writes a real{' '}
          <code>between:…</code> term you can copy, edit, or share.
        </div>
        <div>
          <b>Add a term without typing</b> by expanding a record and clicking any field
          value. Click it again to take it back off.
        </div>
        <div>
          <b>Remove one term</b> with the × on its chip above the timeline.{' '}
          <b>Remove everything</b> with Escape, or “clear all”.
        </div>
        <div>
          <b>Invert any term</b> with a leading minus: <code>-source:nginx</code> is
          everything except nginx.
        </div>
      </div>

      <div class="help-grid">
        {SECTIONS.map((section) => (
          <div class="help-section" key={section.title}>
            <div class="help-title">{section.title}</div>
            {section.items.map(([example, description]) => (
              <div class="help-row" key={example}>
                <button
                  class="help-example"
                  title={`add ${example} to the filter`}
                  onClick={() => onInsert(example)}
                >
                  {example}
                </button>
                <span class="help-desc">{description}</span>
              </div>
            ))}
          </div>
        ))}
      </div>

      <div class="help-foot">
        Bare times are read in <b>{timezone}</b>, the timezone shown in the header.
        A typo'd field name is an error naming the fields that exist, never an empty
        result.
      </div>
    </div>
  );
}
