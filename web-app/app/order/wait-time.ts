/**
 * Phrases the machine's expected_wait_sec for a customer about to order, or
 * null when the machine sent no estimate (no drink made yet, or an older
 * module). Minutes round up so the kiosk never promises a drink too early.
 */
export function waitTimeLabel(waitSec: number | undefined): string | null {
  if (waitSec === undefined || !Number.isFinite(waitSec)) return null;
  if (waitSec < 30) return "No wait — I'll start right away";
  const minutes = Math.ceil(waitSec / 60);
  return `About ${minutes} min wait`;
}
