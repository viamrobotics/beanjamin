"use client";

import { useState } from "react";
import {
  type OrderRecord,
  type Panel,
  type SortKey,
  type SortDir,
  panelTitle,
  panelEmptyMsg,
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

function OrderTable({ orders }: { orders: OrderRecord[] }) {
  const [page, setPage] = useState(0);
  const [sort, setSort] = useState<{ key: SortKey; dir: SortDir }>({
    key: "time",
    dir: "desc",
  });

  const sorted = [...orders].sort((a, b) => compareOrders(a, b, sort));
  const pageCount = Math.ceil(orders.length / ORDERS_PER_PAGE);
  const pageRows = sorted.slice(
    page * ORDERS_PER_PAGE,
    page * ORDERS_PER_PAGE + ORDERS_PER_PAGE
  );

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
          </tr>
        </thead>
        <tbody>
          {pageRows.map((o) => (
            <tr
              key={o.orderId || o.startTime.toISOString()}
              className="border-t border-neutral-200"
            >
              <td className="px-2 py-1">
                {o.startTime.toLocaleTimeString(undefined, {
                  hour: "numeric",
                  minute: "2-digit",
                })}
              </td>
              <td className="px-2 py-1">{o.customerName || "—"}</td>
              <td className="px-2 py-1">{o.drink || "—"}</td>
              <td className="px-2 py-1">
                {o.durationMs
                  ? `${(o.durationMs / 1000).toFixed(1)}s`
                  : "—"}
              </td>
              <td className="px-2 py-1">
                {o.ok ? (
                  <span className="text-green-600">OK</span>
                ) : (
                  <span className="text-red-500" title={o.errorMessage}>
                    {o.errorMessage || "Failed"}
                  </span>
                )}
              </td>
            </tr>
          ))}
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
}: {
  panel: Panel;
  orders: OrderRecord[] | null;
  error: string | null;
  onClose: () => void;
}) {
  return (
    <div className="mt-4 p-4 border border-neutral-200 rounded-lg bg-neutral-50">
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
      ) : (
        <OrderTable orders={orders} />
      )}
    </div>
  );
}
