"use client";

import {
  useEffect,
  useState,
  useRef,
  useCallback,
  type ReactNode,
  type PointerEvent as ReactPointerEvent,
} from "react";
import {
  getQueue,
  cancelOrder,
  type ViamConnection,
  type QueueOrder,
} from "../lib/viamClient";
import { drinkLabel } from "./drinks";

// --- Step display rules -------------------------------------------------
//
// The Go module records the raw espresso step label per order and owns
// the full lifecycle: pending → being made → cleanup → completed_at set →
// pruned after ~15s. The frontend just renders Status().orders in order.
//
// Render rules per order:
//   - completed_at non-empty       → green "Ready!" card
//   - raw_step in cleanup set      → green "Ready to pick up" card
//   - first pending, raw_step set  → amber dot + raw_step label
//   - first pending, no raw_step   → amber dot + "Making..."
//   - other pending                → "In queue · #N"
//
// Orders the backend marks `cancellable` can be swiped left to reveal a
// Cancel action; the machine decides which those are (never the one it is
// already making), so the card is only the messenger.

const READY_RAW_STEPS = new Set<string>([
  "Grabbing filter",
  "Unlocking portafilter",
  "Cleaning",
  "Finishing up",
]);

const READY_LABEL = "Ready to pick up";
const DONE_LABEL = "Ready!";
const QUEUED_LABEL = "Making...";

const GREEN_CARD_CLASSES =
  "anim-slide-in rounded-2xl bg-white border-2 border-emerald-300 px-5 py-4";
const NORMAL_CARD_CLASSES =
  "anim-slide-in rounded-2xl bg-white border border-neutral-200 px-5 py-4";

// How far a card slides left, in px, to fully reveal the Cancel action.
const CANCEL_PANE_WIDTH = 104;
// Past this much travel the row snaps open on release instead of closing.
const SNAP_THRESHOLD = CANCEL_PANE_WIDTH / 2;
// Below this much horizontal movement the gesture is still ambiguous, so the
// row neither moves nor claims the pointer — a near-vertical drag stays a
// scroll of the list.
const DRAG_SLOP = 6;

const ERROR_DISMISS_MS = 4000;
// How long a cancelled order stays hidden before its entry is forgotten —
// long enough to outlive any poll that was already in flight.
const CANCEL_HIDE_MS = 3000;

function isCompleted(order: QueueOrder): boolean {
  return Boolean(order.completed_at);
}

function isReadyDuringCleanup(order: QueueOrder): boolean {
  return READY_RAW_STEPS.has(order.raw_step);
}

/**
 * Whether this card offers the swipe-to-cancel action. The backend's
 * `cancellable` flag is authoritative; the fallback covers machines running a
 * module version that predates it, where "not completed and not the front
 * pending order" is the same rule the tracker already renders by.
 */
function isCancellable(
  order: QueueOrder,
  index: number,
  firstPendingIdx: number,
): boolean {
  if (order.cancellable !== undefined) return order.cancellable;
  return !isCompleted(order) && index !== firstPendingIdx;
}

// The backend's refusals are operator-facing ("use 'cancel' to stop the
// machine"); the kiosk says the same thing to a customer.
function cancelErrorMessage(err: unknown): string {
  const raw = err instanceof Error ? err.message : String(err);
  if (raw.includes("already being made")) {
    return "Too late — that one's already being made.";
  }
  if (raw.includes("already been made")) {
    return "That drink is already done.";
  }
  if (raw.includes("no such order")) {
    return "That order has already left the queue.";
  }
  return "Couldn't cancel that order. Try again.";
}

interface OrderTrackerProps {
  viamConn: ViamConnection | null;
  onEmpty: () => void;
  // When true, stay mounted on an empty queue (and suppress onEmpty). Used by
  // the manual "View queue" takeover so it doesn't auto-dismiss.
  persistent?: boolean;
  // Optional × button in the header.
  onClose?: () => void;
}

export function OrderTracker({ viamConn, onEmpty, persistent, onClose }: OrderTrackerProps) {
  const [orders, setOrders] = useState<QueueOrder[]>([]);
  // ID → when its cancel was sent. Hidden from the list straight away so the
  // card doesn't linger for up to a poll interval after the tap. Entries age
  // out rather than clearing as soon as the backend stops reporting the
  // order: a poll already in flight when the cancel landed answers with the
  // order still queued, and would flash the card back.
  const [cancelling, setCancelling] = useState<Map<string, number>>(new Map());
  // At most one row shows its Cancel action at a time.
  const [openId, setOpenId] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  // Don't fire onEmpty until we've seen at least one order — otherwise the
  // tracker dismisses itself immediately after mount because prepareOrder is
  // fired asynchronously from placeOrder and hasn't enqueued yet.
  const hasSeenOrders = useRef(false);

  const poll = useCallback(async () => {
    if (!viamConn) return;
    try {
      const q = await getQueue(viamConn);
      setOrders(q.orders);
      setCancelling((prev) => {
        if (prev.size === 0) return prev;
        const cutoff = Date.now() - CANCEL_HIDE_MS;
        const next = new Map([...prev].filter(([, sentAt]) => sentAt > cutoff));
        return next.size === prev.size ? prev : next;
      });
      if (q.orders.length > 0) {
        hasSeenOrders.current = true;
      }
    } catch (err) {
      console.error("[order-tracker] poll failed:", err);
    }
  }, [viamConn]);

  // Poll every second
  useEffect(() => {
    if (!viamConn) return;
    // poll() is async: setOrders runs after `await getQueue`, so it is not the
    // synchronous in-effect setState (cascading render) this rule guards against.
    // eslint-disable-next-line react-hooks/set-state-in-effect
    poll();
    const interval = setInterval(poll, 1000);
    return () => clearInterval(interval);
  }, [viamConn, poll]);

  useEffect(() => {
    if (!error) return;
    const t = setTimeout(() => setError(null), ERROR_DISMISS_MS);
    return () => clearTimeout(t);
  }, [error]);

  const handleCancel = useCallback(
    async (order: QueueOrder) => {
      if (!viamConn) return;
      setOpenId(null);
      setCancelling((prev) => new Map(prev).set(order.id, Date.now()));
      try {
        await cancelOrder(viamConn, order.id);
        setError(null);
        // Refresh now rather than waiting out the poll interval, so the queue
        // positions behind the cancelled order renumber immediately.
        await poll();
      } catch (err) {
        console.error("[order-tracker] cancel failed:", err);
        setError(cancelErrorMessage(err));
        setCancelling((prev) => {
          const next = new Map(prev);
          next.delete(order.id);
          return next;
        });
      }
    },
    [viamConn, poll],
  );

  // Orders with a cancel in flight are already gone as far as the list is
  // concerned, including for the "is the queue empty?" question below.
  const visible = orders.filter((o) => !cancelling.has(o.id));

  // Notify parent when the backend has nothing left to show — persistent
  // trackers stay mounted and own their own dismiss via onClose.
  useEffect(() => {
    if (persistent) return;
    if (hasSeenOrders.current && visible.length === 0) {
      onEmpty();
    }
  }, [visible.length, onEmpty, persistent]);

  if (visible.length === 0 && !persistent) return null;

  // Pending count for the header chip — orders that haven't completed yet.
  const pendingCount = visible.filter((o) => !isCompleted(o)).length;
  // Index of the first pending order, used to decide which card gets the
  // amber "currently being made" treatment vs. "in queue".
  const firstPendingIdx = visible.findIndex((o) => !isCompleted(o));

  return (
    <div className="h-full flex flex-col bg-neutral-50 p-6">
      <div className="flex items-center justify-between mb-6">
        <h2 className="text-sm font-mono font-semibold text-neutral-400 uppercase tracking-widest">
          Orders
          <span className="ml-2 inline-flex items-center justify-center w-6 h-6 rounded-full bg-neutral-200 text-neutral-600 text-xs font-bold">
            {pendingCount}
          </span>
        </h2>
        {onClose && (
          <button
            onClick={onClose}
            aria-label="Close queue"
            className="text-neutral-400 hover:text-neutral-700 text-xl leading-none px-2"
          >
            ×
          </button>
        )}
      </div>

      {error && (
        <p
          role="status"
          className="mb-3 rounded-xl bg-red-50 border border-red-200 px-4 py-2 text-xs font-mono text-red-600"
        >
          {error}
        </p>
      )}

      {visible.length === 0 ? (
        <div className="flex-1 flex items-center justify-center">
          <p className="text-sm font-mono text-neutral-400 uppercase tracking-widest">
            No active orders
          </p>
        </div>
      ) : (
        <div className="flex-1 overflow-y-auto space-y-3">
          {visible.map((order, i) => {
            const card = renderCard(order, i, firstPendingIdx);
            if (!isCancellable(order, i, firstPendingIdx)) {
              return <div key={order.id}>{card}</div>;
            }
            return (
              <SwipeToCancel
                key={order.id}
                open={openId === order.id}
                onOpenChange={(open) => setOpenId(open ? order.id : null)}
                onCancel={() => handleCancel(order)}
                label={`Cancel ${order.customer_name || "order"}`}
              >
                {card}
              </SwipeToCancel>
            );
          })}
        </div>
      )}
    </div>
  );
}

function renderCard(
  order: QueueOrder,
  i: number,
  firstPendingIdx: number,
): ReactNode {
  if (isCompleted(order)) {
    return (
      <OrderCard
        order={order}
        cardClass={GREEN_CARD_CLASSES}
        statusKind="ready"
        label={DONE_LABEL}
      />
    );
  }
  if (isReadyDuringCleanup(order)) {
    return (
      <OrderCard
        order={order}
        cardClass={GREEN_CARD_CLASSES}
        statusKind="ready"
        label={READY_LABEL}
      />
    );
  }
  if (i === firstPendingIdx) {
    return (
      <OrderCard
        order={order}
        cardClass={NORMAL_CARD_CLASSES}
        statusKind="making"
        label={order.raw_step || QUEUED_LABEL}
      />
    );
  }
  // Other pending orders: position relative to the front pending order.
  return (
    <OrderCard
      order={order}
      cardClass={NORMAL_CARD_CLASSES}
      statusKind="queued"
      label={`In queue · #${i - firstPendingIdx + 1}`}
    />
  );
}

interface SwipeToCancelProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  onCancel: () => void;
  /** Accessible name for the revealed button. */
  label: string;
  children: ReactNode;
}

/**
 * Slide-to-reveal wrapper: dragging the card left uncovers a Cancel button
 * sitting behind it. The row is vertically transparent — `touch-action: pan-y`
 * leaves scrolling the order list to the browser, and a gesture is only
 * claimed once it has travelled DRAG_SLOP horizontally.
 */
function SwipeToCancel({
  open,
  onOpenChange,
  onCancel,
  label,
  children,
}: SwipeToCancelProps) {
  // Non-null only while a swipe is in progress. The rest of the time the row
  // sits wherever the parent's `open` says it should, so its position is never
  // a second copy of that state waiting to drift out of sync.
  const [dragOffset, setDragOffset] = useState<number | null>(null);
  // Null while no pointer is down. `claimed` flips once the drag has passed
  // the slop and the row started following the finger.
  const drag = useRef<{ x: number; from: number; claimed: boolean } | null>(null);
  // A drag ends with a click event on the card. Without this the click would
  // immediately shut the row the swipe just opened.
  const swallowClick = useRef(false);

  const offset = dragOffset ?? (open ? CANCEL_PANE_WIDTH : 0);

  const handlePointerDown = (e: ReactPointerEvent<HTMLDivElement>) => {
    drag.current = { x: e.clientX, from: offset, claimed: false };
  };

  const handlePointerMove = (e: ReactPointerEvent<HTMLDivElement>) => {
    const d = drag.current;
    if (!d) return;
    const dx = d.x - e.clientX;
    if (!d.claimed) {
      if (Math.abs(dx) < DRAG_SLOP) return;
      d.claimed = true;
      e.currentTarget.setPointerCapture(e.pointerId);
    }
    setDragOffset(Math.min(CANCEL_PANE_WIDTH, Math.max(0, d.from + dx)));
  };

  const endDrag = () => {
    const d = drag.current;
    drag.current = null;
    setDragOffset(null);
    if (!d?.claimed) return;
    swallowClick.current = true;
    onOpenChange(offset > SNAP_THRESHOLD);
  };

  // A tap (no drag) on an open row puts it back, so a customer who swiped by
  // accident isn't left staring at a red button.
  const handleClick = () => {
    if (swallowClick.current) {
      swallowClick.current = false;
      return;
    }
    if (open) onOpenChange(false);
  };

  return (
    <div className="relative overflow-hidden rounded-2xl">
      <button
        type="button"
        onClick={onCancel}
        aria-label={label}
        tabIndex={open ? 0 : -1}
        className="absolute inset-y-0 right-0 flex items-center justify-center bg-red-500 text-white text-xs font-mono font-semibold uppercase tracking-widest active:bg-red-600"
        style={{ width: CANCEL_PANE_WIDTH }}
      >
        Cancel
      </button>
      <div
        onPointerDown={handlePointerDown}
        onPointerMove={handlePointerMove}
        onPointerUp={endDrag}
        onPointerCancel={endDrag}
        onClick={handleClick}
        // Positioned so the card paints over the absolutely-placed Cancel
        // button behind it rather than under it.
        className="relative"
        style={{
          transform: `translateX(-${offset}px)`,
          transition:
            dragOffset === null
              ? "transform 200ms cubic-bezier(0.23, 1, 0.32, 1)"
              : "none",
          touchAction: "pan-y",
        }}
      >
        {children}
      </div>
    </div>
  );
}

interface OrderCardProps {
  order: QueueOrder;
  cardClass: string;
  statusKind: "ready" | "making" | "queued";
  label: string;
}

function OrderCard({ order, cardClass, statusKind, label }: OrderCardProps) {
  const drink = drinkLabel(order.drink);
  return (
    <div className={cardClass}>
      <div className="flex items-baseline justify-between gap-2">
        <p
          className="text-lg text-neutral-900 truncate"
          style={{ fontFamily: "var(--font-just-me), cursive" }}
        >
          {order.modified_customer_name || order.customer_name}
        </p>
        <span className="text-[10px] font-mono text-neutral-300 shrink-0">
          {order.id.slice(0, 8)}
        </span>
      </div>
      {drink && (
        <p className="text-xs font-mono text-neutral-500 mt-0.5">
          {drink}
        </p>
      )}
      {statusKind === "ready" ? (
        <div className="flex items-center gap-2 mt-1">
          <span className="text-emerald-500 text-sm">&#10003;</span>
          <span className="text-xs font-mono font-medium text-emerald-600 uppercase tracking-wider">
            {label}
          </span>
        </div>
      ) : statusKind === "making" ? (
        <div className="flex items-center gap-2 mt-1">
          <span className="pulse-making inline-block w-2 h-2 rounded-full bg-amber-500" />
          <span className="text-xs font-mono font-medium text-amber-600 uppercase tracking-wider">
            {label}
          </span>
        </div>
      ) : (
        <div className="flex items-center justify-between gap-2 mt-1">
          <p className="text-xs font-mono text-neutral-400 uppercase tracking-wider">
            {label}
          </p>
          {order.cancellable && (
            <span
              aria-hidden
              className="text-[10px] font-mono text-neutral-300 tracking-tighter shrink-0"
            >
              ‹ swipe
            </span>
          )}
        </div>
      )}
    </div>
  );
}
