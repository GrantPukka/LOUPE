import { useCallback, useEffect, useState } from 'preact/hooks';
import { browse, subscribe, unsubscribe } from './api.js';

/**
 * The directory picker.
 *
 * Navigation is confined to the roots the server offers — home, the working
 * directory, /var/log, and anything already subscribed. That confinement lives
 * on the server, not here; this only renders what it is allowed to see.
 */
export function Browser({ open, onClose, onChanged }) {
  const [path, setPath] = useState('');
  const [listing, setListing] = useState(null);
  const [error, setError] = useState(null);
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState('');

  const load = useCallback(async (target) => {
    setBusy(true);
    try {
      setListing(await browse(target));
      setPath(target ?? '');
      setError(null);
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy(false);
    }
  }, []);

  useEffect(() => {
    if (open) load('');
  }, [open, load]);

  if (!open) return null;

  const act = async (fn, target) => {
    setBusy(true);
    try {
      const result = await fn(target);
      setNote(result.note ?? '');
      await load(path);
      onChanged?.(result);
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy(false);
    }
  };

  const atRoots = !listing?.path;

  return (
    <div class="modal-backdrop" onClick={onClose}>
      <div class="modal" onClick={(e) => e.stopPropagation()}>
        <div class="modal-head">
          <span>Add a log location</span>
          <button class="clear" onClick={onClose}>
            close
          </button>
        </div>

        {!atRoots && (
          <div class="crumbs">
            <button class="crumb" onClick={() => load('')} title="back to the starting points">
              roots
            </button>
            {listing.parent && (
              <button class="crumb" onClick={() => load(listing.parent)}>
                ↑ up
              </button>
            )}
            <span class="crumb-path">{listing.path}</span>

            {listing.subscribed ? (
              <button class="unsub" onClick={() => act(unsubscribe, listing.path)}>
                unsubscribe
              </button>
            ) : (
              <button class="sub" onClick={() => act(subscribe, listing.path)}>
                subscribe to this folder
              </button>
            )}
          </div>
        )}

        {error && <div class="error-bar">{error}</div>}
        {note && <div class="modal-note">{note}</div>}

        <div class="modal-body">
          {busy && !listing && <div class="empty">loading…</div>}

          {atRoots &&
            (listing?.roots ?? []).map((root) => (
              <div class="browse-row" key={root.path} onClick={() => load(root.path)}>
                <span class="browse-icon">▸</span>
                <span class="browse-name">{root.label}</span>
                <span class="browse-meta">{root.path}</span>
              </div>
            ))}

          {!atRoots &&
            (listing?.entries ?? []).map((entry) => (
              <div
                class={`browse-row ${entry.is_dir ? '' : 'file'}`}
                key={entry.path}
                onClick={() => entry.is_dir && load(entry.path)}
              >
                <span class="browse-icon">{entry.is_dir ? '▸' : '·'}</span>
                <span class="browse-name">{entry.name}</span>

                <span class="browse-meta">
                  {entry.is_dir
                    ? entry.log_count > 0 && `${entry.log_count} log file(s)`
                    : entry.looks_like_log
                      ? bytes(entry.size)
                      : ''}
                </span>

                {entry.subscribed && <span class="browse-tag">subscribed</span>}
                {entry.is_dir && !entry.subscribed && entry.log_count > 0 && (
                  <button
                    class="sub"
                    onClick={(e) => {
                      e.stopPropagation();
                      act(subscribe, entry.path);
                    }}
                  >
                    subscribe
                  </button>
                )}
              </div>
            ))}

          {!atRoots && listing?.entries?.length === 0 && (
            <div class="empty">This folder is empty.</div>
          )}
        </div>

        <div class="modal-foot">
          loupe reads these files; it never writes to them. Subscribing only remembers the
          path.
        </div>
      </div>
    </div>
  );
}

/**
 * The subscription list, with a control to stop reading one.
 *
 * An unsubscribed location keeps its cached records for 14 days, which the note
 * says out loud — otherwise "unsubscribe" reads like "delete".
 */
export function Subscriptions({ open, onClose, subs, onChanged }) {
  const [busy, setBusy] = useState(false);
  const [note, setNote] = useState('');
  const [error, setError] = useState(null);

  if (!open) return null;

  const act = async (fn, path) => {
    setBusy(true);
    try {
      const result = await fn(path);
      setNote(result.note ?? '');
      onChanged?.(result);
    } catch (e) {
      setError(e.message);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div class="modal-backdrop" onClick={onClose}>
      <div class="modal" onClick={(e) => e.stopPropagation()}>
        <div class="modal-head">
          <span>Subscribed locations</span>
          <button class="clear" onClick={onClose}>
            close
          </button>
        </div>

        {error && <div class="error-bar">{error}</div>}
        {note && <div class="modal-note">{note}</div>}

        <div class="modal-body">
          {(subs?.subscriptions ?? []).length === 0 && (
            <div class="empty">
              Nothing subscribed yet.{'\n'}
              <span>Add a folder, or run `loupe subscribe /var/log`.</span>
            </div>
          )}

          {(subs?.subscriptions ?? []).map((s) => (
            <div class="browse-row" key={s.path}>
              <span class="browse-icon">{s.active ? '●' : '○'}</span>
              <span class="browse-name">{s.name}</span>
              <span class="browse-meta">{s.path}</span>

              {/* A subscription the running session never loaded is not yet on
                  screen, and saying so beats letting someone wonder where the
                  records are. */}
              {s.active && !s.loaded && <span class="browse-warn">restart to load</span>}
              {!s.active && <span class="browse-tag">unsubscribed {s.removed_at}</span>}

              {s.active && (
                <button class="unsub" disabled={busy} onClick={() => act(unsubscribe, s.path)}>
                  unsubscribe
                </button>
              )}
              {!s.active && (
                <button class="sub" disabled={busy} onClick={() => act(subscribe, s.path)}>
                  resubscribe
                </button>
              )}
            </div>
          ))}
        </div>

        <div class="modal-foot">
          Unsubscribing keeps the cached records for 14 days, so resubscribing is instant.
          Both actions are written to the audit trail.
        </div>
      </div>
    </div>
  );
}

function bytes(n) {
  if (!n) return '';
  const units = ['B', 'KiB', 'MiB', 'GiB'];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  return `${n.toFixed(i ? 1 : 0)}${units[i]}`;
}
