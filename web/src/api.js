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

/**
 * Open the live record stream.
 *
 * Server-sent events rather than a websocket: the traffic is one-way, an
 * EventSource reconnects on its own, and it needs nothing the browser does not
 * already have. The server holds one follower for the whole process rather
 * than one per connection, so a second tab costs a subscriber, not a second
 * pass over the files.
 *
 * Returns a close function. Callers must call it — an EventSource left open
 * keeps the server polling the log directory for a page nobody is watching.
 */
export function openTail({ filter, onRecords, onNotice, onError }) {
  const source = new EventSource(`/api/tail?filter=${encodeURIComponent(filter ?? '')}`);

  source.addEventListener('records', (e) => {
    try {
      onRecords?.(JSON.parse(e.data));
    } catch {
      // A malformed frame is not worth tearing the stream down for; the next
      // one will almost certainly parse.
    }
  });

  // A source that stopped being readable, or a poll that failed. Reported,
  // never swallowed: a live tail that has quietly stopped covering one file is
  // the failure this project exists to avoid.
  source.addEventListener('notice', (e) => {
    try {
      onNotice?.(JSON.parse(e.data).message);
    } catch {
      onNotice?.('the live stream reported a problem it could not describe');
    }
  });

  // The stream dropped updates because this client fell behind. The records
  // are still in the store, so the honest response is to say so and re-query,
  // not to pretend the tail is complete.
  source.addEventListener('lag', (e) => {
    try {
      onError?.(JSON.parse(e.data).message);
    } catch {
      onError?.('the live stream fell behind. Re-run the filter to catch up.');
    }
  });

  source.onerror = () => {
    // EventSource retries by itself. Only a closed connection is terminal.
    if (source.readyState === EventSource.CLOSED) {
      onError?.('The live stream disconnected. Is `loupe serve` still running?');
    }
  };

  return () => source.close();
}

/**
 * Fetch the message templates matching a filter.
 *
 * The server returns session.PatternSet unchanged, so the rail and
 * `loupe patterns` are looking at the same numbers — including what a limit
 * hid, which the rail has to state rather than stopping quietly.
 */
export const getPatterns = ({ filter, limit, newSince } = {}) => {
  const params = new URLSearchParams();
  if (filter) params.set('filter', filter);
  if (limit !== undefined) params.set('limit', String(limit));
  if (newSince) params.set('new_since', newSince);

  const query = params.toString();
  return request(`/api/patterns${query ? `?${query}` : ''}`);
};

/**
 * Fetch the value breakdown of one field.
 *
 * The server returns session.TopSet unchanged, including the share of each
 * value and how many matching records carry no value at all. The browser must
 * not recompute either: a percentage the UI derived itself could disagree with
 * the one `loupe top` prints.
 */
export const getTop = ({ field, filter, limit } = {}) => {
  const params = new URLSearchParams({ field: field ?? '' });
  if (filter) params.set('filter', filter);
  if (limit !== undefined) params.set('limit', String(limit));
  return request(`/api/top?${params}`);
};
