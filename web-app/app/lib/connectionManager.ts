import { connectToViam, type ViamConnection } from "./viamClient";

// Upper bound on a single dial. Without this, a stuck SDK signaling dial would
// wedge in the pool as a promise that never settles, blocking recovery.
export const DIAL_TIMEOUT_MS = 10_000;

export function withTimeout<T>(
  p: Promise<T>,
  ms: number,
  label: string
): Promise<T> {
  return Promise.race([
    p,
    new Promise<T>((_, reject) =>
      setTimeout(() => reject(new Error(`${label} timed out after ${ms}ms`)), ms)
    ),
  ]);
}

/**
 * Pools Viam connections keyed by `partId`. A single manager can back one
 * connection (the kiosk, via `useViamConnection`) or many (the dashboard, one
 * per online machine). Centralizing it keeps the dial-timeout, in-flight
 * dedup, and teardown behavior identical for every caller.
 */
export interface ConnectionManager {
  /**
   * Return a live connection for `partId`, dialing if one isn't already
   * cached. Concurrent calls for the same `partId` share a single in-flight
   * dial. Rejects (and evicts the cached promise) on dial failure/timeout so
   * the next call re-dials rather than replaying the failure.
   */
  get(partId: string): Promise<ViamConnection>;
  /**
   * Drop the cached connection for `partId` and fire-and-forget its teardown.
   * The next `get` re-dials. Use this when a channel looks dead (e.g. a failed
   * heartbeat or repeated RPC errors).
   */
  invalidate(partId: string): void;
  /** Tear down every cached connection (call on unmount). */
  closeAll(): void;
}

export function createConnectionManager(): ConnectionManager {
  const entries = new Map<string, Promise<ViamConnection>>();

  function get(partId: string): Promise<ViamConnection> {
    const existing = entries.get(partId);
    if (existing) return existing;

    const dialed = withTimeout(
      connectToViam(partId),
      DIAL_TIMEOUT_MS,
      "dial"
    ).catch((err) => {
      // Evict the rejected promise so the next get() re-dials instead of
      // replaying the same failure forever. Guard against clobbering a newer
      // entry that may have replaced this one in the meantime.
      if (entries.get(partId) === dialed) entries.delete(partId);
      throw err;
    });
    entries.set(partId, dialed);
    return dialed;
  }

  function invalidate(partId: string): void {
    const entry = entries.get(partId);
    if (!entry) return;
    entries.delete(partId);
    // Fire-and-forget teardown of the (possibly zombie) channel. Awaiting a
    // stuck disconnect would block recovery, and a fresh get() re-dials.
    void entry
      .then((conn) => {
        if (!conn.isDev) void conn.robotClient.disconnect().catch(() => {});
      })
      .catch(() => {});
  }

  function closeAll(): void {
    for (const partId of [...entries.keys()]) invalidate(partId);
  }

  return { get, invalidate, closeAll };
}
