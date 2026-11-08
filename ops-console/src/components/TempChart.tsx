"use client";
// Streaming temperature chart (uPlot, canvas) with threshold bands and
// pause/resume — accessibility: current value + trend summary text nearby.
import { useEffect, useRef, useState } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";

type Point = { ts: string; temp: number | null };

export function TempChart({
  points,
  setpoint,
  paused,
}: {
  points: Point[];
  setpoint: number;
  paused: boolean;
}) {
  const wrap = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);

  useEffect(() => {
    if (!wrap.current || points.length === 0) return;
    const xs = points.map((p) => new Date(p.ts).getTime() / 1000);
    const ys = points.map((p) => p.temp);
    const alarm = xs.map(() => -10);
    const risk = xs.map(() => -14);
    const sp = xs.map(() => setpoint);
    const data: uPlot.AlignedData = [xs, ys, alarm, risk, sp];

    const opts: uPlot.Options = {
      width: wrap.current.clientWidth,
      height: 260,
      scales: { x: { time: true } },
      axes: [
        { stroke: "#94a3b8", grid: { stroke: "#272f4288" } },
        { stroke: "#94a3b8", grid: { stroke: "#272f4288" } },
      ],
      series: [
        {},
        {
          label: "Temp °C",
          stroke: "#38bdf8",
          width: 1.6,
          fill: "rgba(56,189,248,0.12)",
          value: (_u, v) => (v == null ? "--" : `${v.toFixed(1)}°C`),
        },
        { label: "Alarm −10°C", stroke: "#ef4444", width: 1, dash: [6, 4] },
        { label: "At-risk −14°C", stroke: "#f59e0b", width: 1, dash: [6, 4] },
        { label: "Setpoint", stroke: "#22c55e", width: 1, dash: [2, 4] },
      ],
    };

    if (!plot.current) {
      plot.current = new uPlot(opts, data, wrap.current);
    } else if (!paused) {
      plot.current.setData(data);
    }
    const onResize = () => {
      plot.current?.setSize({ width: wrap.current?.clientWidth ?? 0, height: 260 });
    };
    window.addEventListener("resize", onResize);
    return () => window.removeEventListener("resize", onResize);
  }, [points, paused, setpoint]);

  useEffect(() => {
    return () => {
      plot.current?.destroy();
      plot.current = null;
    };
  }, []);

  const last = points[points.length - 1]?.temp;
  const prev = points[Math.max(0, points.length - 20)]?.temp;
  const trend =
    last != null && prev != null
      ? last > prev + 0.05
        ? "rising"
        : last < prev - 0.05
          ? "falling"
          : "stable"
      : "no data";

  return (
    <div>
      <div className="mb-1 flex items-center justify-between text-[11px] text-[#94a3b8]">
        <span aria-live="polite">
          current: {last != null ? `${last.toFixed(1)}°C` : "--"} · trend {trend}
          {paused && <span className="ml-2 text-[#f59e0b]">(paused)</span>}
        </span>
      </div>
      <div ref={wrap} className="w-full" />
    </div>
  );
}
