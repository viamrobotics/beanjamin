"use client";

import {
  type DailyOrderCount,
  type RobotDayRow,
  formatDay,
} from "./data";

const ROBOT_COLORS = [
  "#93c5fd",
  "#fca5a5",
  "#86efac",
  "#fdba74",
  "#c4b5fd",
  "#a5f3fc",
  "#fde68a",
];

export function OrdersChart({
  data,
  onBarClick,
  selected,
}: {
  data: DailyOrderCount[];
  onBarClick: (day: Date, row: RobotDayRow) => void;
  selected: { dayMs: number; robotId: string } | null;
}) {
  const days = [...data].slice(0, 7).reverse();
  const robotNames = [
    ...new Set(days.flatMap((d) => d.rows.map((r) => r.robotName))),
  ].sort();
  const colorByRobot = new Map(
    robotNames.map((name, i) => [name, ROBOT_COLORS[i % ROBOT_COLORS.length]])
  );
  const totalByRobot = new Map<string, number>();
  for (const d of days) {
    for (const r of d.rows) {
      totalByRobot.set(r.robotName, (totalByRobot.get(r.robotName) ?? 0) + r.count);
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
                const h = yMax === 0 ? 0 : (plotH * count) / yMax;
                const x = groupX + ri * barW;
                const y = margin.top + plotH - h;
                const isSelected =
                  selected !== null &&
                  row !== undefined &&
                  selected.dayMs === d.day.getTime() &&
                  selected.robotId === row.robotId;
                return (
                  <g key={name}>
                    <rect
                      x={x + 1}
                      y={y}
                      width={Math.max(0, barW - 2)}
                      height={h}
                      fill={colorByRobot.get(name)}
                      stroke={isSelected ? "#374151" : "none"}
                      strokeWidth={isSelected ? 2 : 0}
                      rx={2}
                      className={row && count > 0 ? "cursor-pointer" : "cursor-default"}
                      onClick={() => {
                        if (row && count > 0) onBarClick(d.day, row);
                      }}
                    />
                    {count > 0 && (
                      <text
                        x={x + barW / 2}
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
      </svg>

      <div className="text-sm mt-2">
        <div className="text-neutral-500 mb-1">Last 7 days</div>
        <div className="flex flex-wrap gap-3">
          {robotNames.map((name) => (
            <span key={name} className="inline-flex items-center gap-1.5">
              <span
                className="inline-block w-3 h-3 rounded-sm"
                style={{ background: colorByRobot.get(name) }}
              />
              {name}: <strong>{totalByRobot.get(name) ?? 0}</strong>
            </span>
          ))}
        </div>
      </div>
    </div>
  );
}
