"use client";
// Device detail: live chart, twin panel, controls, command history, trace link.
import { use, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "@/lib/api";
import { datadogTraceURL } from "@/lib/datadog";
import { StatusPill } from "@/components/StatusPill";
import { TempChart } from "@/components/TempChart";
import { ArrowSquareOut, Pause, Play } from "@phosphor-icons/react";

const ACTIONS = [
  { id: "compressor_restart", label: "Compressor restart", danger: false },
  { id: "defrost", label: "Defrost cycle", danger: false },
  { id: "reboot", label: "Reboot device", danger: true },
];

function ago(ts?: string | null): string {
  if (!ts) return "never";
  return `${Math.max(0, Math.round((Date.now() - new Date(ts).getTime()) / 1000))}s ago`;
}

export default function DevicePage({ params }: { params: Promise<{ deviceID: string }> }) {
  const { deviceID } = use(params);
  const qc = useQueryClient();
  const [paused, setPaused] = useState(false);
  const pausedRef = useRef(paused);
  pausedRef.current = paused;

  const device = useQuery({
    queryKey: ["device", deviceID],
    queryFn: () => api.device(deviceID),
    refetchInterval: 5000,
  });
  // chart keeps its own history buffer so refetches append smoothly
  const series = useQuery({
    queryKey: ["series", deviceID],
    queryFn: () => api.series(deviceID, 15),
    refetchInterval: 5000,
  });
  // Warm/cold tier history: the $merge rollups (telemetry_hourly/daily), not
  // raw telemetry — the data that outlives the 24h TTL. Slow-moving, so no
  // poll: invalidate on tier change is enough.
  const [tier, setTier] = useState<"hourly" | "daily">("hourly");
  const history = useQuery({
    queryKey: ["history", deviceID, tier],
    queryFn: () => api.history(deviceID, tier, tier === "hourly" ? 7 : 30),
  });
  const histPoints = useMemo(
    () => (history.data ?? []).map((p) => ({ ts: p.bucket, temp: p.avgTempC })),
    [history.data],
  );
  const commands = useQuery({
    queryKey: ["commands", deviceID],
    queryFn: async () =>
      (await fetch(`${process.env.NEXT_PUBLIC_OPS_API ?? "http://localhost:8092"}/api/events?device=${deviceID}&limit=10`).then((r) => r.json())) as never[],
    refetchInterval: 10000,
  });
  const incidents = useQuery({
    queryKey: ["incidents"],
    queryFn: () => api.incidents(),
    refetchInterval: 10000,
  });

  const dev = device.data ?? {};
  const status = (dev.status as string) ?? "STALE";
  const reported = (dev.reported as Record<string, unknown>) ?? {};
  const setpoint = Number(dev.setpoint ?? -18);
  const traceId = (incidents.data ?? []).find((i) => i.deviceId === deviceID)?.traceId;
  const traceURL = datadogTraceURL(traceId);

  const act = async (action: string) => {
    await api.command(deviceID, action);
    qc.invalidateQueries();
  };

  return (
    <div className="space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="font-mono text-lg font-semibold">{deviceID}</h1>
        <StatusPill status={status as "OK" | "AT_RISK" | "ALARM" | "STALE"} />
        <span className="text-[12px] text-[#94a3b8]">
          {String(dev.site ?? "")} · {String(dev.zone ?? "")} · {String(dev.product ?? "")} ·
          value at risk ${Number(dev.valueAtRisk ?? 0).toLocaleString()}
        </span>
        <a
          href={traceURL}
          target="_blank"
          rel="noreferrer"
          className="ml-auto inline-flex items-center gap-1 text-[12px] text-[#38bdf8] hover:underline"
        >
          <ArrowSquareOut className="h-3.5 w-3.5" aria-hidden /> Open trace in Datadog
        </a>
      </div>

      <div className="grid gap-4 lg:grid-cols-[2fr_1fr]">
        <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3">
          <div className="mb-2 flex items-center justify-between">
            <h2 className="text-sm font-semibold">Temperature (last 15 min)</h2>
            <button
              onClick={() => setPaused((p) => !p)}
              className="inline-flex cursor-pointer items-center gap-1 rounded border border-[#475569] px-2 py-0.5 text-[11px] hover:bg-white/10"
              aria-pressed={paused}
            >
              {paused ? <Play className="h-3 w-3" aria-hidden /> : <Pause className="h-3 w-3" aria-hidden />}
              {paused ? "Resume" : "Pause"}
            </button>
          </div>
          <TempChart points={series.data ?? []} setpoint={setpoint} paused={paused} />
        </section>

        <div className="space-y-4">
          <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3">
            <h2 className="mb-2 text-sm font-semibold">Device Twin</h2>
            <dl className="grid grid-cols-2 gap-x-3 gap-y-1 font-mono text-[12px]">
              <dt className="text-[#94a3b8]">reported temp</dt>
              <dd>{reported.tempC != null ? `${Number(reported.tempC).toFixed(1)}°C` : "--"}</dd>
              <dt className="text-[#94a3b8]">compressor</dt>
              <dd>{reported.compressorRunning ? "running" : "stopped"}</dd>
              <dt className="text-[#94a3b8]">door</dt>
              <dd>{reported.doorOpen ? "OPEN" : "closed"}</dd>
              <dt className="text-[#94a3b8]">power</dt>
              <dd>{reported.powerDraw != null ? `${Number(reported.powerDraw).toFixed(0)} W` : "--"}</dd>
              <dt className="text-[#94a3b8]">desired setpoint</dt>
              <dd>{setpoint.toFixed(1)}°C</dd>
              <dt className="text-[#94a3b8]">last seen</dt>
              <dd>{ago(dev.lastSeen as string | null)}</dd>
            </dl>
          </section>

          <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3">
            <h2 className="mb-2 text-sm font-semibold">Controls</h2>
            <div className="flex flex-wrap gap-2">
              {ACTIONS.map((a) => (
                <button
                  key={a.id}
                  onClick={() => act(a.id)}
                  className={`cursor-pointer rounded px-3 py-1.5 text-sm font-medium transition-opacity hover:opacity-90 ${
                    a.danger ? "bg-[#ef4444] text-white" : "border border-[#475569] hover:bg-white/10"
                  }`}
                >
                  {a.label}
                </button>
              ))}
            </div>
          </section>

          <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3">
            <h2 className="mb-2 text-sm font-semibold">Recent Events</h2>
            <ul className="space-y-1 font-mono text-[11px]">
              {(commands.data ?? []).length === 0 && <li className="text-[#94a3b8]">none</li>}
              {(commands.data ?? []).map((e: { type: string; message: string; at: string }, i) => (
                <li key={i} className="flex gap-2">
                  <span className="text-[#94a3b8]">{new Date(e.at).toLocaleTimeString()}</span>
                  <span>{e.type}</span>
                  <span className="truncate text-[#94a3b8]">{e.message}</span>
                </li>
              ))}
            </ul>
          </section>
        </div>
      </div>

      <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3">
        <div className="mb-2 flex flex-wrap items-center gap-3">
          <h2 className="text-sm font-semibold">
            History — {tier === "hourly" ? "warm tier (hourly rollups)" : "cold tier (daily rollups)"}
          </h2>
          {/* tier toggle: hourly = telemetry_hourly (7d), daily = telemetry_daily (365d) */}
          <div className="flex rounded border border-[#475569] text-[11px]">
            {(["hourly", "daily"] as const).map((t) => (
              <button
                key={t}
                onClick={() => setTier(t)}
                aria-pressed={tier === t}
                className={`cursor-pointer px-2 py-0.5 ${
                  tier === t ? "bg-[#38bdf8]/20 text-[#38bdf8]" : "text-[#94a3b8] hover:bg-white/10"
                }`}
              >
                {t}
              </button>
            ))}
          </div>
          {history.data && history.data.length > 0 && (
            <span className="font-mono text-[11px] text-[#94a3b8]">
              {history.data.length} buckets · raw readings expired (24h TTL), summaries kept{" "}
              {tier === "hourly" ? "7d" : "365d"} · newest {history.data[history.data.length - 1].bucketLocal} ICT
            </span>
          )}
          {history.data && history.data.length === 0 && (
            <span className="text-[11px] text-[#94a3b8]">
              no summary buckets yet — run{" "}
              <code className="font-mono">go run ./tools/seedhistory</code> to pre-seed demo history
            </span>
          )}
        </div>
        {/* History is not live: passing paused would freeze uPlot before the
            tier switch's new data arrives, so hourly stayed on screen after
            selecting daily. */}
        <TempChart points={histPoints} setpoint={setpoint} paused={false} />
      </section>
    </div>
  );
}
