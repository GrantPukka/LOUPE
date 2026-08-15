// The API client.
//
// Every call goes to the same endpoints `loupe serve` exposes, which in turn
// call the same query path the CLI uses. There is no query logic here: the UI
// sends a filter string and renders what comes back, so it cannot drift from
// what the terminal would show.

/** Raised for a non-2xx response, carrying the server's full message. */
export class ApiError extends Error {
  constructor(message, status) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

async function request(path, options) {
  let response;
  try {
    response = await fetch(path, options);
  } catch (cause) {
    throw new ApiError(
      `Could not reach loupe at ${location.host}. Is \`loupe serve\` still running?`,
      0,
    );
  }

  const body = await response.json().catch(() => null);

  if (!response.ok) {
    // The server sends the CLI's error verbatim — a spelling suggestion, a
    // caret pointing at the problem, a working example. Pass all of it on.
    throw new ApiError(body?.error ?? `Request failed (${response.status})`, response.status);
  }
  return body;
}

const post = (path, payload) =>
  request(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  });

export const getSchema = () => request('/api/schema');

export const runQuery = (payload) => post('/api/query', payload);

export const getHistogram = (payload) => post('/api/histogram', payload);

/**
 * Columns for the record list.
 *
 * seq is fetched but never displayed: it identifies a row so that expanding one
 * can fetch its detail, and so the list can tell two identical-looking records
 * apart. The CLI's default selection deliberately omits it, so the UI asks for
 * it explicitly rather than widening the shared default.
 */
export const LIST_COLUMNS = 'seq, ts, level, source, message';

/** Columns fetched when a row is expanded, rather than for every row. */
export const DETAIL_COLUMNS =
  'seq, ts, ts_zoned, level, message, source, file, format, line_no, parsed, raw, fields';

/**
 * Fetch one record in full.
 *
 * seq is a real column, so this reuses the ordinary filter path rather than
 * needing an endpoint of its own.
 */
export const getRecord = (seq) =>
  post('/api/query', { filter: `seq:${seq}`, columns: DETAIL_COLUMNS, limit: 1 });

export const browse = (path) =>
  request(`/api/browse?path=${encodeURIComponent(path ?? '')}`);

export const getSubscriptions = () => request('/api/subscriptions');

export const subscribe = (path, label) => post('/api/subscribe', { path, label: label ?? '' });

export const unsubscribe = (path) => post('/api/unsubscribe', { path });
