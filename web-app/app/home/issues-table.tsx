"use client";

import { useState } from "react";
import { type IssueReportRecord } from "./data";
import { DrinkName } from "../order/drink-name";
import {
  OTHER_ISSUE,
  SEVERITIES,
  issueLabel,
  severityChipClass,
  severityLabel,
} from "../order/issues";

function SeverityChip({ severity }: { severity: string }) {
  return (
    <span
      className={`inline-block px-2 py-0.5 rounded-full border text-xs font-medium ${severityChipClass(severity)}`}
    >
      {severityLabel(severity || "unknown")}
    </span>
  );
}

function formatWhen(d: Date): string {
  return d.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
}

export function IssuesTable({
  issues,
  machineNameById,
  windowDays,
}: {
  issues: IssueReportRecord[] | null;
  machineNameById: Map<string, string>;
  windowDays: number;
}) {
  const [severityFilter, setSeverityFilter] = useState<string | null>(null);

  const counts = new Map<string, number>();
  for (const i of issues ?? []) {
    counts.set(i.severity, (counts.get(i.severity) ?? 0) + 1);
  }
  const shown = (issues ?? []).filter(
    (i) => severityFilter === null || i.severity === severityFilter
  );

  return (
    <div className="mt-6">
      <div className="flex flex-wrap items-baseline justify-between gap-2 mb-1">
        <h3 className="text-lg font-semibold text-neutral-900">
          Reported issues
        </h3>
        {issues && issues.length > 0 && (
          <div className="flex flex-wrap gap-2">
            {[...SEVERITIES].reverse().map((s) => (
              <button
                key={s}
                type="button"
                onClick={() =>
                  setSeverityFilter((f) => (f === s ? null : s))
                }
                aria-pressed={severityFilter === s}
                className={`px-3 py-1 text-sm rounded-md border transition-colors tabular-nums ${
                  severityFilter === s
                    ? "border-neutral-900 bg-neutral-900 text-white"
                    : "border-neutral-200 bg-white text-neutral-900 hover:bg-neutral-100"
                }`}
              >
                {severityLabel(s)} · {counts.get(s) ?? 0}
              </button>
            ))}
          </div>
        )}
      </div>
      <p className="text-sm text-neutral-500 mb-3">
        Problems customers reported from the kiosk over the last {windowDays}{" "}
        days, including on orders that otherwise succeeded.
      </p>
      {issues === null ? (
        <p className="text-neutral-500">Loading reported issues…</p>
      ) : issues.length === 0 ? (
        <p className="text-neutral-500">No issues reported.</p>
      ) : (
        <div className="overflow-x-auto rounded-md border border-neutral-200">
          <table className="w-full text-sm">
            <thead className="bg-neutral-50 text-left text-neutral-500">
              <tr>
                <th className="px-3 py-2 font-medium">When</th>
                <th className="px-3 py-2 font-medium">Machine</th>
                <th className="px-3 py-2 font-medium">Severity</th>
                <th className="px-3 py-2 font-medium">Issue</th>
                <th className="px-3 py-2 font-medium">Order</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((i, n) => {
                return (
                  <tr
                    key={`${i.orderId}-${i.reportedAt.getTime()}-${n}`}
                    className="border-t border-neutral-200 align-top"
                  >
                    <td className="px-3 py-2 whitespace-nowrap tabular-nums">
                      {formatWhen(i.reportedAt)}
                    </td>
                    <td className="px-3 py-2 whitespace-nowrap">
                      {machineNameById.get(i.robotId) ?? i.robotId}
                    </td>
                    <td className="px-3 py-2">
                      <SeverityChip severity={i.severity} />
                    </td>
                    <td className="px-3 py-2">
                      <div className="font-medium">
                        {i.issue === OTHER_ISSUE ? "Other" : issueLabel(i.issue)}
                      </div>
                      {i.description && (
                        <div className="text-neutral-600 max-w-md">
                          {i.description}
                        </div>
                      )}
                    </td>
                    <td className="px-3 py-2">
                      {i.order ? (
                        <>
                          <div>
                            <DrinkName
                              drink={i.order.drink}
                              beans={i.order.beans}
                            />
                            {!i.order.ok && (
                              <span className="ml-2 text-xs text-red-600">
                                failed
                              </span>
                            )}
                          </div>
                          <div className="text-neutral-500">
                            {i.order.customerName || "Anonymous"}
                          </div>
                        </>
                      ) : (
                        <div className="text-neutral-400">
                          Order not synced yet
                        </div>
                      )}
                      <div
                        className="text-xs text-neutral-400 font-mono truncate max-w-[12rem]"
                        title={i.orderId}
                      >
                        {i.orderId}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
