"use client";

import { useEffect, useRef, useState } from "react";
import * as VIAM from "@viamrobotics/sdk";
import {
  type OrderRecord,
  type Panel,
  type SortKey,
  type SortDir,
  panelTitle,
  panelEmptyMsg,
  loadVideosForOrder,
  getVideoSignedUrl,
  countVideosForOrders,
} from "./data";

const PAGER_BUTTON =
  "px-3 py-1 rounded-md border border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-100 transition-colors disabled:bg-neutral-50 disabled:text-neutral-400 disabled:cursor-not-allowed";

const ORDERS_PER_PAGE = 5;

const ORDER_COLUMNS = [
  { key: "time", label: "Time", defaultDir: "desc" },
  { key: "customer", label: "Customer", defaultDir: "asc" },
  { key: "drink", label: "Drink", defaultDir: "asc" },
  { key: "duration", label: "Duration", defaultDir: "desc" },
  { key: "status", label: "Status", defaultDir: "asc" },
] as const;

const TABLE_COL_COUNT = ORDER_COLUMNS.length + 1; // +1 for the video column

type VideoEntry =
  | { state: "loading" }
  | { state: "error"; message: string }
  | {
      state: "ready";
      items: { id: string; url: string; capturedAt: Date | null }[];
    };

function formatDuration(ms: number): string {
  if (ms <= 0) return "—";
  const totalSeconds = Math.round(ms / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes === 0) return `${seconds}s`;
  return `${minutes}m ${seconds}s`;
}

function compareOrders(
  a: OrderRecord,
  b: OrderRecord,
  sort: { key: SortKey; dir: SortDir }
): number {
  const sign = sort.dir === "desc" ? -1 : 1;
  switch (sort.key) {
    case "time":
      return sign * (a.startTime.getTime() - b.startTime.getTime());
    case "customer":
      return sign * a.customerName.localeCompare(b.customerName);
    case "drink":
      return sign * a.drink.localeCompare(b.drink);
    case "duration":
      return sign * (a.durationMs - b.durationMs);
    case "status":
      return sign * (Number(a.ok) - Number(b.ok));
  }
}

function VideoExpansion({ entry }: { entry: VideoEntry | undefined }) {
  if (!entry || entry.state === "loading") {
    return <p className="text-neutral-500 m-0">Loading video…</p>;
  }
  if (entry.state === "error") {
    return <p className="text-red-500 m-0">Error: {entry.message}</p>;
  }
  if (entry.items.length === 0) {
    return (
      <p className="text-neutral-500 m-0">No clip available yet.</p>
    );
  }
  return (
    <div className="space-y-2">
      {entry.items.map((item) => (
        <video
          key={item.id}
          controls
          src={item.url}
          className="max-w-full rounded-md border border-neutral-200"
        />
      ))}
    </div>
  );
}

function OrderTable({
  orders,
  viamClient,
  videoCountByOrder,
}: {
  orders: OrderRecord[];
  viamClient: VIAM.ViamClient | null;
  videoCountByOrder: Map<string, number>;
}) {
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({
    key: "time",
    dir: "desc",
  });
  const [expandedOrder, setExpandedOrder] = useState<string | null>(null);
  const [videoByOrder, setVideoByOrder] = useState<Map<string, VideoEntry>>(
    new Map()
  );

  const sorted = [...orders].sort((a, b) => compareOrders(a, b, sort));
  const pageCount = Math.ceil(orders.length / ORDERS_PER_PAGE);
  const pageRows = sorted.slice(
    page * ORDERS_PER_PAGE,
    page * ORDERS_PER_PAGE + ORDERS_PER_PAGE
  );

  const toggleVideo = (order: OrderRecord) => {
    const orderId = order.orderId;
    if (expandedOrder === orderId) {
      setExpandedOrder(null);
      return;
    }
    setExpandedOrder(orderId);
    if (videoByOrder.has(orderId) || !viamClient) return;
    setVideoByOrder((prev) => new Map(prev).set(orderId, { state: "loading" }));
    (async () => {
      try {
        const videos = await loadVideosForOrder(viamClient, order);
        const items = await Promise.all(
          videos.map(async (v) => ({
            id: v.binaryDataId,
            url: await getVideoSignedUrl(viamClient, v.binaryDataId),
            capturedAt: v.capturedAt,
          }))
        );
        setVideoByOrder((prev) =>
          new Map(prev).set(orderId, { state: "ready", items })
        );
      } catch (e) {
        console.error("failed to load videos:", e);
        const message = e instanceof Error ? e.message : String(e);
        setVideoByOrder((prev) =>
          new Map(prev).set(orderId, { state: "error", message })
        );
      }
    })();
  };

  return (
    <>
      <table className="w-full border-collapse text-sm">
        <thead>
          <tr className="text-left text-neutral-500">
            {ORDER_COLUMNS.map((col) => {
              const active = sort.key === col.key;
              const arrow = active ? (sort.dir === "desc" ? "↓" : "↑") : "";
              return (
                <th
                  key={col.key}
                  className="px-2 py-1 cursor-pointer select-none font-medium hover:text-neutral-900 transition-colors"
                  onClick={() => {
                    setSort((s) =>
                      s.key === col.key
                        ? { key: col.key, dir: s.dir === "desc" ? "asc" : "desc" }
                        : { key: col.key, dir: col.defaultDir }
                    );
                    setPage(0);
                  }}
                >
                  {col.label} {arrow}
                </th>
              );
            })}
            <th className="px-2 py-1 font-medium">Video</th>
          </tr>
        </thead>
        <tbody>
          {pageRows.flatMap((o) => {
            const rowKey = o.orderId || o.startTime.toISOString();
            const canWatch = !!o.orderId;
            const isExpanded = canWatch && expandedOrder === o.orderId;
            const rows = [
              <tr key={rowKey} className="border-t border-neutral-200">
                <td className="px-2 py-1">
                  {o.startTime.toLocaleTimeString(undefined, {
                    hour: "numeric",
                    minute: "2-digit",
                  })}
                </td>
                <td className="px-2 py-1">{o.customerName || "—"}</td>
                <td className="px-2 py-1">{o.drink || "—"}</td>
                <td className="px-2 py-1">{formatDuration(o.durationMs)}</td>
                <td className="px-2 py-1">
                  {o.ok ? (
                    <span className="text-green-600">OK</span>
                  ) : (
                    <span className="text-red-500" title={o.errorMessage}>
                      {o.errorMessage || "Failed"}
                    </span>
                  )}
                </td>
                <td className="px-2 py-1">
                  {(() => {
                    if (!canWatch) return "—";
                    const count = videoCountByOrder.get(o.orderId);
                    if (isExpanded) {
                      return (
                        <button
                          onClick={() => toggleVideo(o)}
                          className="text-blue-600 hover:underline"
                        >
                          Hide
                        </button>
                      );
                    }
                    if (count === undefined || count === null) {
                      return (
                        <span className="text-neutral-400">▶ …</span>
                      );
                    }
                    if (count === 0) {
                      return <span className="text-neutral-400">no clip</span>;
                    }
                    return (
                      <button
                        onClick={() => toggleVideo(o)}
                        className="text-blue-600 hover:underline"
                      >
                        ▶ Watch ({count})
                      </button>
                    );
                  })()}
                </td>
              </tr>,
            ];
            if (isExpanded) {
              rows.push(
                <tr
                  key={`${rowKey}-video`}
                  className="border-t border-neutral-200 bg-white"
                >
                  <td colSpan={TABLE_COL_COUNT} className="px-2 py-3">
                    <VideoExpansion entry={videoByOrder.get(o.orderId)} />
                  </td>
                </tr>
              );
            }
            return rows;
          })}
        </tbody>
      </table>
      {orders.length > ORDERS_PER_PAGE && (
        <div className="flex items-center justify-between mt-3 text-sm">
          <button
            onClick={() => setPage((p) => Math.max(0, p - 1))}
            disabled={page === 0}
            className={PAGER_BUTTON}
          >
            ← Prev
          </button>
          <span className="text-neutral-500">
            Page {page + 1} of {pageCount} · {orders.length} orders
          </span>
          <button
            onClick={() => setPage((p) => Math.min(pageCount - 1, p + 1))}
            disabled={page >= pageCount - 1}
            className={PAGER_BUTTON}
          >
            Next →
          </button>
        </div>
      )}
    </>
  );
}

export function OrdersPanel({
  panel,
  orders,
  error,
  onClose,
  viamClient,
}: {
  panel: Panel;
  orders: OrderRecord[] | null;
  error: string | null;
  onClose: () => void;
  viamClient: VIAM.ViamClient | null;
}) {
  const panelRef = useRef<HTMLDivElement>(null);
  const hasScrolledRef = useRef(false);
  const [videoCounts, setVideoCounts] = useState<Map<string, number> | null>(
    null
  );
  const loaded = orders !== null || error !== null;
  const noTable = orders === null || orders.length === 0 || error !== null;
  const needsCounts = !!orders && orders.some((o) => !!o.orderId);
  const tableReady = !needsCounts || videoCounts !== null;
  const ready = loaded && (noTable || tableReady);

  useEffect(() => {
    if (!viamClient || !orders) return;
    const targets = orders.filter((o) => !!o.orderId);
    if (targets.length === 0) return;
    let cancelled = false;
    countVideosForOrders(viamClient, targets)
      .then((counts) => {
        if (!cancelled) setVideoCounts(counts);
      })
      .catch((e) => {
        console.error("failed to count videos:", e);
        if (!cancelled) setVideoCounts(new Map());
      });
    return () => {
      cancelled = true;
    };
  }, [orders, viamClient]);

  useEffect(() => {
    if (ready && !hasScrolledRef.current) {
      hasScrolledRef.current = true;
      panelRef.current?.scrollIntoView({
        behavior: "smooth",
        block: "start",
      });
    }
  }, [ready]);
  return (
    <div
      ref={panelRef}
      className="mt-4 p-4 border border-neutral-200 rounded-lg bg-neutral-50"
    >
      <div className="flex justify-between items-center mb-3">
        <strong className="text-neutral-900">{panelTitle(panel)}</strong>
        <button
          onClick={onClose}
          className="border-none bg-transparent cursor-pointer text-neutral-500 hover:text-neutral-900 transition-colors text-lg leading-none"
          aria-label="Close"
        >
          ×
        </button>
      </div>
      {error ? (
        <p className="text-red-500">Error: {error}</p>
      ) : orders === null ? (
        <p className="text-neutral-500">Loading orders…</p>
      ) : orders.length === 0 ? (
        <p className="text-neutral-500">{panelEmptyMsg(panel)}</p>
      ) : !tableReady ? (
        <p className="text-neutral-500">Loading orders…</p>
      ) : (
        <OrderTable
          orders={orders}
          viamClient={viamClient}
          videoCountByOrder={videoCounts ?? new Map()}
        />
      )}
    </div>
  );
}
