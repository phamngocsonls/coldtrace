"use client";
// Fleet Overview: KPI strip + live device grid + event feed + AI activity.
import Link from "next/link";
import { useQuery } from "@tanstack/react-query";
import { api, type DeviceRow, type EventRow, type Incident, type SiteInsight } from "@/lib/api";
import { StatusPill } from "@/components/StatusPill";
import { DemoDrawer } from "@/components/DemoDrawer";

function ago(ts?: string | null): string {
  if (!ts) return "never";
  const s = Math.max(0, Math.round((Date.now() - new Date(ts).getTime()) / 1000));
  return `${s}s ago`;
}

function Kpi({ label, value, tone = "" }: { label: string; value: string | number; tone?: string }) {
  return (
    <div className="rounded-lg border border-[#475569]/50 bg-[#1b2336] px-3 py-2">
      <div className="text-[11px] font-medium uppercase tracking-wide text-[#94a3b8]">{label}</div>
      <div className={`font-mono text-xl font-semibold ${tone}`}>{value}</div>
    </div>
  );
}

function TempTile({ d }: { d: DeviceRow }) {
  const temp = d.reported?.tempC;
  return (
    <Link
      href={`/devices/${d.deviceId}`}
      className="group rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3 transition-colors hover:border-[#22c55e]/60"
    >
      <div className="flex items-center justify-between">
        <span className="font-mono text-sm font-semibold">{d.deviceId}</span>
        <StatusPill status={d.status} />
      </div>
      <div className="mt-1 truncate text-[11px] text-[#94a3b8]">
        {d.site} · {d.zone} · {d.product}
      </div>
      <div className="mt-2 flex items-baseline gap-2">
        <span
          className={`font-mono text-2xl font-semibold ${
            d.status === "ALARM" ? "text-[#ef4444]" : d.status === "AT_RISK" ? "text-[#f59e0b]" : ""
          }`}
        >
          {temp != null ? `${temp.toFixed(1)}°` : "--"}
        </span>
        <span className="text-[11px] text-[#94a3b8]">setpoint {d.setpoint}°</span>
      </div>
      <div className="mt-1 text-[10px] text-[#94a3b8]">updated {ago(d.lastSeen)}</div>
    </Link>
  );
}

export default function FleetPage() {
  const fleet = useQuery({ queryKey: ["fleet"], queryFn: api.fleet, refetchInterval: 15000 });
  const events = useQuery({ queryKey: ["events"], queryFn: () => api.events(12), refetchInterval: 10000 });
  const incidents = useQuery({ queryKey: ["incidents"], queryFn: () => api.incidents("OPEN"), refetchInterval: 10000 });
  const sites = useQuery({ queryKey: ["sites"], queryFn: api.siteInsights, refetchInterval: 30000 });

  const rows = fleet.data ?? [];
  const ok = rows.filter((d) => d.status === "OK").length;
  const risk = rows.filter((d) => d.status === "AT_RISK").length;
  const alarm = rows.filter((d) => d.status === "ALARM").length;
  const varAtRisk = rows
    .filter((d) => d.status === "ALARM" || d.status === "AT_RISK")
    .reduce((s, d) => s + (d.valueAtRisk ?? 0), 0);
  const aiResolved = (incidents.data ?? []).filter((i) => i.remediatedBy === "ai-agent").length;

  return (
    <div className="space-y-4">
      <div className="flex items-center justify-between">
        <h1 className="text-lg font-semibold">Fleet Overview</h1>
        <DemoDrawer />
      </div>

      <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 lg:grid-cols-6">
        <Kpi label="Online" value={`${ok + risk + alarm}/${rows.length}`} />
        <Kpi label="OK" value={ok} tone="text-[#22c55e]" />
        <Kpi label="At Risk" value={risk} tone={risk ? "text-[#f59e0b]" : ""} />
        <Kpi label="Alarm" value={alarm} tone={alarm ? "text-[#ef4444]" : ""} />
        <Kpi label="Open Incidents" value={incidents.data?.length ?? 0} />
        <Kpi label="Value at Risk" value={`$${varAtRisk.toLocaleString()}`} tone={varAtRisk ? "text-[#ef4444]" : ""} />
      </div>

      {(sites.data?.length ?? 0) > 0 && (
        <section aria-label="Sites" className="space-y-2">
          <h2 className="text-sm font-semibold">Sites</h2>
          <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3">
            {(sites.data ?? []).map((s: SiteInsight) => (
              <div
                key={s.site}
                className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3"
              >
                <div className="flex items-center justify-between">
                  <span className="font-mono text-sm font-semibold">{s.site}</span>
                  <span className="text-[11px] text-[#94a3b8]">{s.devices} devices</span>
                </div>
                <div className="mt-2 flex items-center gap-3 font-mono text-[12px]">
                  <span className="text-[#22c55e]">{s.ok} OK</span>
                  <span className={s.atRisk ? "text-[#f59e0b]" : "text-[#94a3b8]"}>{s.atRisk} risk</span>
                  <span className={s.alarm ? "text-[#ef4444]" : "text-[#94a3b8]"}>{s.alarm} alarm</span>
                </div>
                <div className="mt-1.5 flex items-center gap-3 text-[11px] text-[#94a3b8]">
                  <span>{s.openIncidents} open inc.</span>
                  <span>VAR ${s.valueAtRisk.toLocaleString()}</span>
                  <span>
                    slope {s.maxSlopeCpm >= 0 ? "+" : ""}
                    {s.maxSlopeCpm.toFixed(2)}°/min
                  </span>
                  {s.mttrMin > 0 && <span>MTTR {s.mttrMin.toFixed(0)}m</span>}
                </div>
              </div>
            ))}
          </div>
        </section>
      )}

      <div className="grid gap-4 lg:grid-cols-[2fr_1fr]">
        <section aria-label="Devices">
          <div className="grid grid-cols-2 gap-3 sm:grid-cols-3 xl:grid-cols-4">
            {fleet.isLoading
              ? Array.from({ length: 12 }, (_, i) => (
                  <div key={i} className="h-[120px] animate-pulse rounded-lg bg-[#1b2336]/60" />
                ))
              : rows.map((d) => <TempTile key={d.deviceId} d={d} />)}
          </div>
        </section>

        <div className="space-y-4">
          <section
            aria-label="Live events"
            className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3"
          >
            <h2 className="mb-2 text-sm font-semibold">Live Events</h2>
            <ul className="space-y-1.5 font-mono text-[11px] leading-relaxed">
              {(events.data ?? []).map((e: EventRow, i) => (
                <li key={e.id ?? i} className="flex gap-2">
                  <span className="shrink-0 text-[#94a3b8]">
                    {new Date(e.at).toLocaleTimeString()}
                  </span>
                  <span
                    className={
                      e.severity === "critical"
                        ? "text-[#ef4444]"
                        : e.severity === "warning"
                          ? "text-[#f59e0b]"
                          : "text-[#22c55e]"
                    }
                  >
                    {e.type}
                  </span>
                  <span className="truncate text-[#94a3b8]">{e.message}</span>
                </li>
              ))}
              {!events.data?.length && <li className="text-[#94a3b8]">waiting for events…</li>}
            </ul>
          </section>

          <section
            aria-label="AI activity"
            className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3"
          >
            <div className="mb-2 flex items-center justify-between">
              <h2 className="text-sm font-semibold">AI Activity</h2>
              <Link href="/incidents" className="text-[11px] text-[#38bdf8] hover:underline">
                all incidents →
              </Link>
            </div>
            <ul className="space-y-2 text-[12px]">
              {(incidents.data ?? []).slice(0, 5).map((i: Incident) => (
                <li key={i._id} className="rounded border border-[#475569]/40 p-2">
                  <div className="flex items-center justify-between font-mono text-[11px]">
                    <span className="font-semibold">{i.deviceId}</span>
                    <span className="text-[#94a3b8]">{i.faultClass}</span>
                    <span
                      className={
                        i.decision === "AUTO"
                          ? "text-[#22c55e]"
                          : i.decision === "HOLD"
                            ? "text-[#f59e0b]"
                            : "text-[#38bdf8]"
                      }
                    >
                      {i.decision}
                    </span>
                  </div>
                  <div className="mt-0.5 line-clamp-2 text-[11px] text-[#94a3b8]">{i.reason}</div>
                </li>
              ))}
              {!incidents.data?.length && (
                <li className="text-[#94a3b8]">no open incidents — fleet healthy</li>
              )}
            </ul>
          </section>
        </div>
      </div>
    </div>
  );
}
