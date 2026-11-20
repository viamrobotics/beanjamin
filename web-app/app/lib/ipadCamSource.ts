// Helpers for pinning a browser to the machine's `ipad-camera` component: the
// ?ipadcam= URL parameter, the stable id this browser identifies itself with,
// and the two rejection messages the component sends back.

/** Substrings of the component's error messages (see ipadcamera.go). */
export const LEASE_ERROR = "in use by another source";
export const TOKEN_ERROR = "invalid token";

export function isLeaseError(message: string): boolean {
  return message.includes(LEASE_ERROR);
}

export function isTokenError(message: string): boolean {
  return message.includes(TOKEN_ERROR);
}

export interface IpadCamParam {
  enabled: boolean;
  /** The component's `source_token`; undefined for `?ipadcam=1`. */
  token?: string;
}

/**
 * `?ipadcam=1` turns the camera on with no token. Any other non-empty value
 * other than `0` is the token, so `?ipadcam=<token>` turns it on too. The
 * component refuses a `source_token` of "1" so the two can't be confused.
 */
export function parseIpadCamParam(value: string | null): IpadCamParam {
  if (value === null || value === "" || value === "0") {
    return { enabled: false };
  }
  if (value === "1") return { enabled: true };
  return { enabled: true, token: value };
}

const SOURCE_ID_KEY = "ipadcam_source_id";

interface KeyValueStore {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

function randomId(): string {
  return (
    globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2)
  );
}

/**
 * An id that stays the same across reloads of this browser, so a reloaded
 * kiosk reclaims its own lease at once instead of waiting it out. Falls back to
 * a per-page id when storage is unavailable (private windows can throw).
 */
export function getSourceId(
  store: KeyValueStore | undefined = safeLocalStorage(),
): string {
  try {
    const existing = store?.getItem(SOURCE_ID_KEY);
    if (existing) return existing;
    const id = randomId();
    store?.setItem(SOURCE_ID_KEY, id);
    return id;
  } catch {
    return randomId();
  }
}

function safeLocalStorage(): KeyValueStore | undefined {
  try {
    return typeof localStorage === "undefined" ? undefined : localStorage;
  } catch {
    return undefined;
  }
}
