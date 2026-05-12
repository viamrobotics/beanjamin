"use client";

import { useState } from "react";
import {
  type DailyOrderCount,
  type RobotDayRow,
  formatDay,
} from "./data";

// --- Style tokens ---

const COLORS = {
  outline: "#808080",
  axisText: "#374151",
  gridLine: "#e5e7eb",
  gridText: "#6b7280",
  selectionOutline: "#374151",
  tooltipBg: "#1f2937",
  tooltipText: "#f9fafb",
  successText: "#86efac",
  errorText: "#ff7f7f",
  errorStripe: "rgba(31, 41, 55, 0.45)",
  legendErrorBg: "#e5e7eb",
} as const;

const BAR_RADIUS = 2;
const SELECTION_RADIUS = 2;
const ERROR_STRIPES_ID = "error-stripes";

// --- Color picker ---

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

function colorFor(name: string): string {
  return ROBOT_COLOR_OVERRIDES[name] ?? pastelForRobot(name);
}

// --- Path helpers ---

function topRoundedOutline(
  x: number,
  y: number,
  w: number,
  h: number,
  r: number
): string {
  if (h <= 0 || w <= 0) return "";
  const radius = Math.min(r, w / 2, h);
  return `M${x},${y + h} V${y + radius} Q${x},${y} ${x + radius},${y} H${x + w - radius} Q${x + w},${y} ${x + w},${y + radius} V${y + h}`;
}

function topRoundedRect(
  x: number,
  y: number,
  w: number,
  h: number,
  r: number
): string {
  const open = topRoundedOutline(x, y, w, h, r);
  return open ? `${open} Z` : "";
}

// --- Hover state ---

interface HoverInfo {
  cx: number;
  topY: number;
  name: string;
  day: Date;
  okCount: number;
  errorCount: number;
}

// --- Bar (one robot × one day) ---

interface BarProps {
  row: RobotDayRow | undefined;
  name: string;
  day: Date;
  color: string;
  x: number;
  y: number;
  w: number;
  h: number;
  okH: number;
  errorH: number;
  cx: number;
  count: number;
  okCount: number;
  errorCount: number;
  isSelected: boolean;
  onClick: (day: Date, row: RobotDayRow) => void;
  onHover: (info: HoverInfo | null) => void;
}

function Bar({
  row,
  name,
  day,
  color,
  x,
  y,
  w,
  h,
  okH,
  errorH,
  cx,
  count,
  okCount,
  errorCount,
  isSelected,
  onClick,
  onHover,
}: BarProps) {
  const okPath = topRoundedRect(
    x,
    y + errorH,
    w,
    okH,
    errorCount > 0 ? 0 : BAR_RADIUS
  );
  const errorPath = topRoundedRect(x, y, w, errorH, BAR_RADIUS);
  const clickable = !!row && count > 0;

  return (
    <g>
      {okCount > 0 && (
        <path
          d={okPath}
          fill={color}
          stroke={COLORS.outline}
          strokeWidth={1}
          className="pointer-events-none"
        />
      )}
      {errorCount > 0 && (
        <>
          <path d={errorPath} fill={color} className="pointer-events-none" />
          <path
            d={errorPath}
            fill={`url(#${ERROR_STRIPES_ID})`}
            className="pointer-events-none"
          />
          <path
            d={errorPath}
            fill="none"
            stroke={COLORS.outline}
            strokeWidth={1}
            className="pointer-events-none"
          />
        </>
      )}
      <rect
        x={x}
        y={y}
        width={w}
        height={h}
        fill="transparent"
        className={clickable ? "cursor-pointer" : "cursor-default"}
        onClick={() => {
          if (clickable) onClick(day, row);
        }}
        onMouseEnter={() => {
          if (count > 0) {
            onHover({ cx, topY: y, name, day, okCount, errorCount });
          }
        }}
        onMouseLeave={() => onHover(null)}
      />
      {isSelected && (
        <path
          d={topRoundedOutline(x, y, w, h, SELECTION_RADIUS)}
          fill="none"
          stroke={COLORS.selectionOutline}
          strokeWidth={2}
          className="pointer-events-none"
        />
      )}
      {count > 0 && (
        <text
          x={cx}
          y={y - 4}
          textAnchor="middle"
          fontSize={10}
          fill={COLORS.axisText}
          className="pointer-events-none"
        >
          {count}
        </text>
      )}
    </g>
  );
}

// --- Hover tooltip ---

interface HoverTooltipProps {
  hover: HoverInfo;
  marginTop: number;
  marginLeft: number;
  plotW: number;
}

function HoverTooltip({
  hover,
  marginTop,
  marginLeft,
  plotW,
}: HoverTooltipProps) {
  const tooltipW = 170;
  const tooltipH = 42;
  const padding = 6;
  const desiredX = hover.cx - tooltipW / 2;
  const tooltipX = Math.max(
    marginLeft,
    Math.min(marginLeft + plotW - tooltipW, desiredX)
  );
  const tooltipY = Math.max(marginTop, hover.topY - tooltipH - 8);

  return (
    <g pointerEvents="none">
      <rect
        x={tooltipX}
        y={tooltipY}
        width={tooltipW}
        height={tooltipH}
        fill={COLORS.tooltipBg}
        rx={4}
        opacity={0.95}
      />
      <text
        x={tooltipX + padding}
        y={tooltipY + 16}
        fontSize={11}
        fill={COLORS.tooltipText}
      >
        {hover.name} · {formatDay(hover.day)}
      </text>
      <text
        x={tooltipX + padding}
        y={tooltipY + 32}
        fontSize={11}
        fill={COLORS.tooltipText}
      >
        <tspan fill={COLORS.successText}>{hover.okCount} success</tspan>
        <tspan fill={COLORS.outline}>{" · "}</tspan>
        <tspan fill={COLORS.errorText}>{hover.errorCount} errors</tspan>
      </text>
    </g>
  );
}

// --- Legend ---

interface ChartLegendProps {
  robotNames: string[];
  colorByRobot: Map<string, string>;
  totalByRobot: Map<string, number>;
  errorsByRobot: Map<string, number>;
}

function ChartLegend({
  robotNames,
  colorByRobot,
  totalByRobot,
  errorsByRobot,
}: ChartLegendProps) {
  const sorted = [...robotNames].sort(
    (a, b) => (totalByRobot.get(b) ?? 0) - (totalByRobot.get(a) ?? 0)
  );
  return (
    <div className="text-sm shrink-0 md:w-44">
      <div className="text-neutral-900 font-medium">Last 7 days</div>
      <div className="text-xs text-neutral-500 mb-2">Successes (Errors)</div>
      <ul className="flex flex-col gap-1.5">
        {sorted.map((name) => {
          const errors = errorsByRobot.get(name) ?? 0;
          return (
            <li key={name} className="inline-flex items-center gap-1.5">
              <span
                className="inline-block w-3 h-3 rounded-sm shrink-0"
                style={{ background: colorByRobot.get(name) }}
              />
              <span className="truncate">
                {name}: <strong>{totalByRobot.get(name) ?? 0}</strong>
                {errors > 0 && (
                  <span className="text-neutral-500"> ({errors})</span>
                )}
              </span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// --- Main chart ---

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
    robotNames.map((name) => [name, colorFor(name)])
  );
  const totalByRobot = new Map<string, number>();
  const errorsByRobot = new Map<string, number>();
  for (const d of days) {
    for (const r of d.rows) {
      const okCount = Math.max(0, r.count - r.errorCount);
      totalByRobot.set(r.robotName, (totalByRobot.get(r.robotName) ?? 0) + okCount);
      errorsByRobot.set(
        r.robotName,
        (errorsByRobot.get(r.robotName) ?? 0) + r.errorCount
      );
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
    <div className="flex flex-col md:flex-row md:items-start gap-4">
      <svg
        viewBox={`0 0 ${width} ${height}`}
        className="w-full max-w-200 h-auto"
        role="img"
        aria-label="Orders per robot per day"
      >
        <defs>
          <pattern
            id={ERROR_STRIPES_ID}
            width={6}
            height={6}
            patternUnits="userSpaceOnUse"
            patternTransform="rotate(45)"
          >
            <line
              x1={0}
              y1={0}
              x2={0}
              y2={6}
              stroke={COLORS.errorStripe}
              strokeWidth={3}
            />
          </pattern>
        </defs>

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
                stroke={COLORS.gridLine}
                strokeWidth={1}
              />
              <text
                x={margin.left - 6}
                y={y}
                textAnchor="end"
                dominantBaseline="middle"
                fontSize={11}
                fill={COLORS.gridText}
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
                const isSelected =
                  selected !== null &&
                  row !== undefined &&
                  selected.dayMs === d.day.getTime() &&
                  selected.robotId === row.robotId;
                return (
                  <Bar
                    key={name}
                    row={row}
                    name={name}
                    day={d.day}
                    color={colorByRobot.get(name) ?? ""}
                    x={x + 1}
                    y={y}
                    w={Math.max(0, barW - 2)}
                    h={h}
                    okH={okH}
                    errorH={errorH}
                    cx={x + barW / 2}
                    count={count}
                    okCount={okCount}
                    errorCount={errorCount}
                    isSelected={isSelected}
                    onClick={onBarClick}
                    onHover={setHover}
                  />
                );
              })}
              <text
                x={groupX + innerW / 2}
                y={margin.top + plotH + 16}
                textAnchor="middle"
                fontSize={11}
                fill={COLORS.axisText}
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
          stroke={COLORS.outline}
          strokeWidth={1}
        />

        {hover && (
          <HoverTooltip
            hover={hover}
            marginTop={margin.top}
            marginLeft={margin.left}
            plotW={plotW}
          />
        )}
      </svg>

      <ChartLegend
        robotNames={robotNames}
        colorByRobot={colorByRobot}
        totalByRobot={totalByRobot}
        errorsByRobot={errorsByRobot}
      />
    </div>
  );
}
