/**
 * Merging streamed records into the loaded page.
 *
 * Pure functions, kept out of App so they can be reasoned about on their own.
 * Getting this wrong shows a record twice, or drops one, in the exact view
 * somebody is watching an incident through.
 */

/**
 * Reorder streamed rows into the column order the table is already using.
 *
 * The tail endpoint selects the same columns as the record list, so in
 * practice this is a no-op. It is here because a mismatch would not throw: it
 * would put timestamps in the level column and level in the message column,
 * and the page would look plausible while being wrong.
 */
export function alignRows(incoming, incomingColumns, targetColumns) {
  if (!targetColumns?.length) return incoming ?? [];

  const at = new Map((incomingColumns ?? []).map((name, i) => [name, i]));
  const sameOrder =
    incomingColumns?.length === targetColumns.length &&
    targetColumns.every((name, i) => incomingColumns[i] === name);

  if (sameOrder) return incoming ?? [];

  return (incoming ?? []).map((row) => targetColumns.map((name) => {
    const i = at.get(name);
    return i === undefined ? null : row[i];
  }));
}

/**
 * Put streamed rows at the top of a newest-first list.
 *
 * Rows arrive oldest-first from the server, because that is the order the CLI
 * prints them in and the stream reuses its query. The list is newest-first, so
 * they are reversed on the way in.
 */
export function prependNewest(existing, incoming) {
  if (!incoming?.length) return existing ?? [];
  return [...incoming].reverse().concat(existing ?? []);
}

/**
 * Drop rows already on screen or already held.
 *
 * The stream excludes records it has sent, but a query that ran just after a
 * record landed picked it up too — so the same record can arrive by two honest
 * routes. Showing it twice would make the tool look like it was double-counting
 * an incident, which is the one thing a log reader must never do.
 *
 * Without a seq column there is nothing to compare, so nothing is dropped:
 * a duplicate is bad, but inventing an identity and dropping a real record is
 * worse.
 */
export function withoutDuplicates(incoming, seqAt, ...alreadyHave) {
  if (seqAt === undefined || seqAt < 0) return incoming ?? [];

  const seen = new Set();
  for (const rows of alreadyHave) {
    for (const row of rows ?? []) seen.add(row[seqAt]);
  }
  return (incoming ?? []).filter((row) => !seen.has(row[seqAt]));
}
