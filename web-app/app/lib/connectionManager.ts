import { connectToViam, type ViamConnection } from "./viamClient";

// Upper bound on a single dial. Without this, a stuck SDK signaling dial would
// wedge in the pool as a promise that never settles, blocking recovery.
export const DIAL_TIMEOUT_MS = 10_000;

/**
 * Reject if `p` doesn't settle within `ms`. The timer is always cleared once
 * `p` settles, so a winning promise doesn't leave a dangling timeout alive
 * until `ms` elapses (this runs on every heartbeat/dial).
 */
export function withTimeout<T>(
  p: Promise<T>,
  ms: number,
  label: string
): Promise<T> {
  let timer: ReturnType<typeof setTimeout>;
  const timeout = new Promise<T>((_, reject) => {
    timer = setTimeout(
      () => reject(new Error(`${label} timed out after ${ms}ms`)),
      ms
    );
  });
  return Promise.race([p, timeout]).finally(() => clearTimeout(timer));
}

/**
 * Fire-and-forget teardown of a (possibly zombie) channel. Awaiting a stuck
 * disconnect would only block recovery, and callers always re-dial. No-op in
 * dev mode, whose mock client has no real `disconnect`.
 */
export function disconnectQuietly(conn: ViamConnection): void {
  if (!conn.isDev) void conn.robotClient.disconnect().catch(() => {});
}

/**
 * Pools Viam connections keyed by `partId` — one per online machine. Used by
 * the dashboard, which juggles many machines at once; the kiosk talks to a
 * single machine and dials directly via `useViamConnection`.
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

/**
 * `dial` and `timeoutMs` are injectable so the pooling/eviction/dedup logic can
 * be unit-tested without a real Viam dial; they default to the production
 * dialer and timeout.
 */
export function createConnectionManager(
  dial: (partId: string) => Promise<ViamConnection> = connectToViam,
  timeoutMs: number = DIAL_TIMEOUT_MS
): ConnectionManager {
  const entries = new Map<string, Promise<ViamConnection>>();

  function get(partId: string): Promise<ViamConnection> {
    const existing = entries.get(partId);
    if (existing) return existing;

    const dialed = withTimeout(dial(partId), timeoutMs, "dial").catch((err) => {
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
    void entry.then(disconnectQuietly).catch(() => {});
  }

  function closeAll(): void {
    for (const partId of [...entries.keys()]) invalidate(partId);
  }

  return { get, invalidate, closeAll };
}
