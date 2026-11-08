"use client";
// Events explorer: filterable audit trail + operator-authored alert rules.
import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type AlertRule } from "@/lib/api";

export default function EventsPage() {
  const [device, setDevice] = useState("");
  const [type, setType] = useState("");
  const events = useQuery({
    queryKey: ["events", device, type],
    queryFn: () => api.events(200),
    refetchInterval: 5000,
  });
  const rules = useQuery({ queryKey: ["rules"], queryFn: api.rules, refetchInterval: 15000 });
  const qc = useQueryClient();
  const [draft, setDraft] = useState({ metric: "reported.tempC", op: "gt", threshold: -8, severity: "warning" });

  const addRule = async () => {
    try {
      await api.createRule({
        metric: draft.metric,
        op: draft.op as AlertRule["op"],
        threshold: Number(draft.threshold),
        severity: draft.severity as AlertRule["severity"],
      });
      qc.invalidateQueries({ queryKey: ["rules"] });
    } catch (err) {
      alert(`rule rejected: ${err instanceof Error ? err.message : err}`);
    }
  };

  const rows = (events.data ?? []).filter(
    (e) =>
      (!device || e.deviceId?.includes(device)) &&
      (!type || e.type.toLowerCase().includes(type.toLowerCase())),
  );

  return (
    <div className="grid gap-3 lg:grid-cols-[2fr_1fr]">
      <div className="space-y-3">
        <h1 className="text-lg font-semibold">Event Explorer</h1>
        <div className="flex gap-2">
        <input
          value={device}
          onChange={(e) => setDevice(e.target.value.toUpperCase())}
          placeholder="filter deviceId…"
          className="w-40 rounded border border-[#475569] bg-[#0f172a] px-2 py-1 font-mono text-[12px]"
        />
        <input
          value={type}
          onChange={(e) => setType(e.target.value)}
          placeholder="filter type…"
          className="w-56 rounded border border-[#475569] bg-[#0f172a] px-2 py-1 font-mono text-[12px]"
        />
      </div>
      <div className="overflow-hidden rounded-lg border border-[#475569]/50">
        <table className="w-full font-mono text-[12px]">
          <thead className="bg-[#1b2336] text-left text-[#94a3b8]">
            <tr>
              <th className="px-3 py-2">time</th>
              <th className="px-3 py-2">type</th>
              <th className="px-3 py-2">device</th>
              <th className="px-3 py-2">message</th>
              <th className="px-3 py-2">actor</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((e, i) => (
              <tr key={e.id ?? i} className="border-t border-[#475569]/30 odd:bg-white/[0.02]">
                <td className="px-3 py-1.5 text-[#94a3b8]">{new Date(e.at).toLocaleTimeString()}</td>
                <td className="px-3 py-1.5">
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
                </td>
                <td className="px-3 py-1.5">{e.deviceId || "—"}</td>
                <td className="px-3 py-1.5">{e.message}</td>
                <td className="px-3 py-1.5 text-[#94a3b8]">{e.actor}</td>
              </tr>
            ))}
            {!rows.length && (
              <tr>
                <td colSpan={5} className="px-3 py-6 text-center text-[#94a3b8]">
                  no matching events
                </td>
              </tr>
            )}
          </tbody>
        </table>
      </div>
      </div>

      <aside
        aria-label="Alert rules"
        className="h-fit space-y-3 rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3"
      >
        <h2 className="text-sm font-semibold">Alert Rules</h2>
        <p className="text-[11px] text-[#94a3b8]">
          Operator thresholds — evaluated every 30s against live device twins; matches
          land in this event feed with their severity.
        </p>
        <ul className="space-y-1.5 font-mono text-[11px]">
          {(rules.data ?? []).map((r) => (
            <li
              key={r.ruleId}
              className="flex items-center justify-between gap-2 rounded border border-[#475569]/40 px-2 py-1.5"
            >
              <span>
                {r.metric} {r.op} {r.threshold}
                <span
                  className={`ml-2 rounded px-1 ${
                    r.severity === "critical"
                      ? "bg-[#ef4444]/15 text-[#ef4444]"
                      : r.severity === "warning"
                        ? "bg-[#f59e0b]/15 text-[#f59e0b]"
                        : "bg-white/10 text-[#94a3b8]"
                  }`}
                >
                  {r.severity}
                </span>
              </span>
              <button
                onClick={async () => {
                  await api.deleteRule(r.ruleId);
                  qc.invalidateQueries({ queryKey: ["rules"] });
                }}
                aria-label={`delete rule ${r.ruleId}`}
                className="cursor-pointer text-[#94a3b8] hover:text-[#ef4444]"
              >
                ✕
              </button>
            </li>
          ))}
          {!rules.data?.length && <li className="text-[#94a3b8]">no rules — add one below</li>}
        </ul>
        <div className="space-y-2 border-t border-[#475569]/40 pt-2 text-[12px]">
          <select
            value={draft.metric}
            onChange={(e) => setDraft({ ...draft, metric: e.target.value })}
            className="w-full rounded border border-[#475569] bg-[#0f172a] px-2 py-1 font-mono text-[11px]"
          >
            {["reported.tempC", "reported.powerDraw", "reported.compressorRunning", "lastSeenAgeSec"].map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
          <div className="flex gap-2">
            <select
              value={draft.op}
              onChange={(e) => setDraft({ ...draft, op: e.target.value })}
              className="rounded border border-[#475569] bg-[#0f172a] px-2 py-1 font-mono text-[11px]"
            >
              {["gt", "lt", "eq"].map((o) => (
                <option key={o} value={o}>
                  {o}
                </option>
              ))}
            </select>
            <input
              type="number"
              step="any"
              value={draft.threshold}
              onChange={(e) => setDraft({ ...draft, threshold: Number(e.target.value) })}
              className="min-w-0 flex-1 rounded border border-[#475569] bg-[#0f172a] px-2 py-1 font-mono text-[11px]"
            />
            <select
              value={draft.severity}
              onChange={(e) => setDraft({ ...draft, severity: e.target.value })}
              className="rounded border border-[#475569] bg-[#0f172a] px-2 py-1 font-mono text-[11px]"
            >
              {["info", "warning", "critical"].map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
          </div>
          <button
            onClick={addRule}
            className="w-full cursor-pointer rounded bg-[#22c55e] py-1.5 text-[12px] font-semibold text-[#0f172a] hover:opacity-90"
          >
            Add rule
          </button>
        </div>
      </aside>
    </div>
  );
}
