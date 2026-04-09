"use client";

import { useEffect, useState, useRef, useCallback } from "react";
import { getQueue, type ViamConnection } from "../lib/viamClient";

interface DoneOrder {
  name: string;
  removedAt: number;
}

interface OrderTrackerProps {
  viamConn: ViamConnection | null;
  onEmpty: () => void;
}

export function OrderTracker({ viamConn, onEmpty }: OrderTrackerProps) {
  const [queueOrders, setQueueOrders] = useState<string[]>([]);
  const [doneOrders, setDoneOrders] = useState<DoneOrder[]>([]);
  const prevOrders = useRef<string[]>([]);

  const poll = useCallback(async () => {
    if (!viamConn) return;
    try {
      const q = await getQueue(viamConn);
      const current = q.orders as string[];

      // Detect orders that were in the previous list but are gone now
      const removed = prevOrders.current.filter((n) => !current.includes(n));
      if (removed.length > 0) {
        setDoneOrders((prev) => [
          ...prev,
          ...removed.map((name) => ({ name, removedAt: Date.now() })),
        ]);
      }

      prevOrders.current = current;
      setQueueOrders(current);
    } catch {
      // ignore polling errors
    }
  }, [viamConn]);

  // Poll every 2 seconds
  useEffect(() => {
    if (!viamConn) return;
    poll();
    const interval = setInterval(poll, 2000);
    return () => clearInterval(interval);
  }, [viamConn, poll]);

  // Clean up done orders after 5 seconds
  useEffect(() => {
    if (doneOrders.length === 0) return;
    const timer = setInterval(() => {
      const now = Date.now();
      setDoneOrders((prev) => prev.filter((o) => now - o.removedAt < 5000));
    }, 1000);
    return () => clearInterval(timer);
  }, [doneOrders.length]);

  // Notify parent when queue and done list are both empty
  useEffect(() => {
    if (queueOrders.length === 0 && doneOrders.length === 0) {
      onEmpty();
    }
  }, [queueOrders.length, doneOrders.length, onEmpty]);

  const hasOrders = queueOrders.length > 0 || doneOrders.length > 0;
  if (!hasOrders) return null;

  return (
    <div className="h-full flex flex-col bg-neutral-50 p-6">
      <h2 className="text-sm font-mono font-semibold text-neutral-400 uppercase tracking-widest mb-6">
        Orders
        <span className="ml-2 inline-flex items-center justify-center w-6 h-6 rounded-full bg-neutral-200 text-neutral-600 text-xs font-bold">
          {queueOrders.length}
        </span>
      </h2>

      <div className="flex-1 overflow-y-auto space-y-3">
        {/* Active queue orders */}
        {queueOrders.map((name, i) => (
          <div
            key={`q-${name}-${i}`}
            className="anim-slide-in rounded-2xl bg-white border border-neutral-200 px-5 py-4"
          >
            <p
              className="text-lg text-neutral-900 truncate"
              style={{ fontFamily: "var(--font-just-me), cursive" }}
            >
              {name}
            </p>
            {i === 0 ? (
              <div className="flex items-center gap-2 mt-1">
                <span className="pulse-making inline-block w-2 h-2 rounded-full bg-amber-500" />
                <span className="text-xs font-mono font-medium text-amber-600 uppercase tracking-wider">
                  Making...
                </span>
              </div>
            ) : (
              <p className="text-xs font-mono text-neutral-400 mt-1 uppercase tracking-wider">
                In queue &middot; #{i + 1}
              </p>
            )}
          </div>
        ))}

        {/* Done orders (fading out) */}
        {doneOrders.map((order) => (
          <div
            key={`done-${order.name}-${order.removedAt}`}
            className="anim-fade-out rounded-2xl bg-white border border-emerald-200 px-5 py-4"
          >
            <p
              className="text-lg text-neutral-900 truncate"
              style={{ fontFamily: "var(--font-just-me), cursive" }}
            >
              {order.name}
            </p>
            <div className="flex items-center gap-2 mt-1">
              <span className="text-emerald-500 text-sm">&#10003;</span>
              <span className="text-xs font-mono font-medium text-emerald-600 uppercase tracking-wider">
                Ready!
              </span>
            </div>
          </div>
        ))}
      </div>
    </div>
  );
}
