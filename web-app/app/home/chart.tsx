"use client";

import { useState } from "react";
import {
  type DailyOrderCount,
  type RobotDayRow,
  formatDay,
} from "./data";

interface HoverInfo {
  cx: number;
  topY: number;
  name: string;
  day: Date;
  okCount: number;
  errorCount: number;
}

const ROBOT_COLOR_OVERRIDES: Record<string, string> = {
  Cappuccina: "#c4b5fd",
  AverageJoe: "#93c5fd",
  "Roast-Malone": "#d4a373",
};

// Hue ranges that stay clear of the override colors (purple ~258°,
// blue ~213°, brown/orange ~28°): yellow→cyan and pink→magenta.
const SAFE_HUE_RANGES: Array<[number, number]> = [
  [50, 195],
  [285, 350],
];

function hashName(name: string): number {
  let h = 0;
  for (let i = 0; i < name.length; i++) {
    h = (h * 31 + name.charCodeAt(i)) >>> 0;
  }
  return h;
}

function pastelForRobot(name: string): string {
  const totalSpan = SAFE_HUE_RANGES.reduce(
    (acc, [start, end]) => acc + (end - start),
    0
  );
  let offset = hashName(name) % totalSpan;
  let hue = 0;
  for (const [start, end] of SAFE_HUE_RANGES) {
    const span = end - start;
    if (offset < span) {
      hue = start + offset;
      break;
    }
    offset -= span;
  }
  return `hsl(${hue}, 70%, 82%)`;
}
const ERROR_COLOR = "#ff7f7f";

export function OrdersChart({
  data,
  onBarClick,
  selected,
}: {
  data: DailyOrderCount[];
  onBarClick: (day: Date, row: RobotDayRow) => void;
  selected: { dayMs: number; robotId: string } | null;
}) {
  const [hover, setHover] = useState<HoverInfo | null>(null);
  const days = [...data].slice(0, 7).reverse();
  const robotNames = [
    ...new Set(days.flatMap((d) => d.rows.map((r) => r.robotName))),
  ].sort();
  const colorByRobot = new Map(
    robotNames.map((name) => [
      name,
      ROBOT_COLOR_OVERRIDES[name] ?? pastelForRobot(name),
    ])
  );
  const totalByRobot = new Map<string, number>();
  let totalErrors = 0;
  for (const d of days) {
    for (const r of d.rows) {
      const okCount = Math.max(0, r.count - r.errorCount);
      totalByRobot.set(r.robotName, (totalByRobot.get(r.robotName) ?? 0) + okCount);
      totalErrors += r.errorCount;
    }
  }

  const width = 800;
  const height = 280;
  const margin = { top: 20, right: 16, bottom: 44, left: 40 };
  const plotW = width - margin.left - margin.right;
  const plotH = height - margin.top - margin.bottom;

  const maxCount = Math.max(
    1,
    ...days.flatMap((d) => d.rows.map((r) => r.count))
  );
  const yMax = Math.ceil(maxCount * 1.1);
  const yTicks = 4;

  const groupW = plotW / Math.max(days.length, 1);
  const groupPad = groupW * 0.15;
  const innerW = groupW - groupPad * 2;
  const barW = innerW / Math.max(robotNames.length, 1);

  return (
    <div>
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="w-full max-w-200 h-auto"
        role="img"
        aria-label="Orders per robot per day"
      >
        {Array.from({ length: yTicks + 1 }, (_, i) => {
          const v = Math.round((yMax * i) / yTicks);
          const y = margin.top + plotH - (plotH * i) / yTicks;
          return (
            <g key={i}>
              <line
                x1={margin.left}
                x2={margin.left + plotW}
                y1={y}
                y2={y}
                stroke="#e5e7eb"
                strokeWidth={1}
              />
              <text
                x={margin.left - 6}
                y={y}
                textAnchor="end"
                dominantBaseline="middle"
                fontSize={11}
                fill="#6b7280"
              >
                {v}
              </text>
            </g>
          );
        })}

        {days.map((d, di) => {
          const groupX = margin.left + di * groupW + groupPad;
          return (
            <g key={d.day.getTime()}>
              {robotNames.map((name, ri) => {
                const row = d.rows.find((r) => r.robotName === name);
                const count = row?.count ?? 0;
                const errorCount = row?.errorCount ?? 0;
                const okCount = Math.max(0, count - errorCount);
                const h = yMax === 0 ? 0 : (plotH * count) / yMax;
                const errorH = count === 0 ? 0 : (h * errorCount) / count;
                const okH = h - errorH;
                const x = groupX + ri * barW;
                const y = margin.top + plotH - h;
                const okY = y + errorH;
                const rectX = x + 1;
                const rectW = Math.max(0, barW - 2);
                const isSelected =
                  selected !== null &&
                  row !== undefined &&
                  selected.dayMs === d.day.getTime() &&
                  selected.robotId === row.robotId;
                const cx = x + barW / 2;
                return (
                  <g key={name}>
                    {okCount > 0 && (
                      <rect
                        x={rectX}
                        y={okY}
                        width={rectW}
                        height={okH}
                        fill={colorByRobot.get(name)}
                        rx={2}
                        className="pointer-events-none"
                      />
                    )}
                    {errorCount > 0 && (
                      <rect
                        x={rectX}
                        y={y}
                        width={rectW}
                        height={errorH}
                        fill={ERROR_COLOR}
                        rx={2}
                        className="pointer-events-none"
                      />
                    )}
                    <rect
                      x={rectX}
                      y={y}
                      width={rectW}
                      height={h}
                      fill="transparent"
                      stroke={isSelected ? "#374151" : "none"}
                      strokeWidth={isSelected ? 2 : 0}
                      rx={2}
                      className={row && count > 0 ? "cursor-pointer" : "cursor-default"}
                      onClick={() => {
                        if (row && count > 0) onBarClick(d.day, row);
                      }}
                      onMouseEnter={() => {
                        if (count > 0) {
                          setHover({
                            cx,
                            topY: y,
                            name,
                            day: d.day,
                            okCount,
                            errorCount,
                          });
                        }
                      }}
                      onMouseLeave={() => setHover(null)}
                    />
                    {count > 0 && (
                      <text
                        x={cx}
                        y={y - 4}
                        textAnchor="middle"
                        fontSize={10}
                        fill="#374151"
                        className="pointer-events-none"
                      >
                        {count}
                      </text>
                    )}
                  </g>
                );
              })}
              <text
                x={groupX + innerW / 2}
                y={margin.top + plotH + 16}
                textAnchor="middle"
                fontSize={11}
                fill="#374151"
              >
                {formatDay(d.day)}
              </text>
            </g>
          );
        })}

        <line
          x1={margin.left}
          x2={margin.left + plotW}
          y1={margin.top + plotH}
          y2={margin.top + plotH}
          stroke="#9ca3af"
          strokeWidth={1}
        />

        {hover && (() => {
          const tooltipW = 170;
          const tooltipH = 42;
          const padding = 6;
          const desiredX = hover.cx - tooltipW / 2;
          const minX = margin.left;
          const maxX = margin.left + plotW - tooltipW;
          const tooltipX = Math.max(minX, Math.min(maxX, desiredX));
          const tooltipY = Math.max(margin.top, hover.topY - tooltipH - 8);
          return (
            <g pointerEvents="none">
              <rect
                x={tooltipX}
                y={tooltipY}
                width={tooltipW}
                height={tooltipH}
                fill="#1f2937"
                rx={4}
                opacity={0.95}
              />
              <text
                x={tooltipX + padding}
                y={tooltipY + 16}
                fontSize={11}
                fill="#f9fafb"
              >
                {hover.name} · {formatDay(hover.day)}
              </text>
              <text
                x={tooltipX + padding}
                y={tooltipY + 32}
                fontSize={11}
                fill="#f9fafb"
              >
                <tspan fill="#86efac">{hover.okCount} success</tspan>
                <tspan fill="#9ca3af">{" · "}</tspan>
                <tspan fill={ERROR_COLOR}>{hover.errorCount} errors</tspan>
              </text>
            </g>
          );
        })()}
      </svg>

      <div className="text-sm mt-2">
        <div className="text-neutral-500 mb-1">Last 7 days</div>
        <div className="flex flex-wrap gap-3">
          {[...robotNames]
            .sort(
              (a, b) =>
                (totalByRobot.get(b) ?? 0) - (totalByRobot.get(a) ?? 0)
            )
            .map((name) => (
              <span key={name} className="inline-flex items-center gap-1.5">
                <span
                  className="inline-block w-3 h-3 rounded-sm"
                  style={{ background: colorByRobot.get(name) }}
                />
                {name}: <strong>{totalByRobot.get(name) ?? 0}</strong>
              </span>
            ))}
          <span className="inline-flex items-center gap-1.5">
            <span
              className="inline-block w-3 h-3 rounded-sm"
              style={{ background: ERROR_COLOR }}
            />
            errors: <strong>{totalErrors}</strong>
          </span>
        </div>
      </div>
    </div>
  );
}
