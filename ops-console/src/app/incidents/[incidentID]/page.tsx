"use client";
// Incident detail: AI decision card (guardrail gates), LLM RCA with
// human-approved actions, ack/resolve/feedback workflow.
import { use, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Incident, type RCA, type TopFactor } from "@/lib/api";
import { datadogTraceURL } from "@/lib/datadog";
import { RcaPanel } from "@/components/RcaPanel";
import { ArrowSquareOut } from "@phosphor-icons/react";

export default function IncidentPage({ params }: { params: Promise<{ incidentID: string }> }) {
  const { incidentID } = use(params);
  const qc = useQueryClient();
  const incidents = useQuery({
    queryKey: ["incidents"],
    queryFn: () => api.incidents(),
    refetchInterval: 8000,
  });
  const i = (incidents.data ?? []).find((x) => x._id === incidentID);

  const act = async (fn: () => Promise<unknown>) => {
    await fn();
    qc.invalidateQueries();
  };

  if (!i) {
    return incidents.isLoading ? (
      <p className="text-[#94a3b8]">loading…</p>
    ) : (
      <p className="text-[#94a3b8]">incident not found (may have been cleared)</p>
    );
  }

  const gates: { label: string; pass: boolean | undefined }[] = [
    { label: "class approved for autonomy", pass: i.decision !== "PROPOSED" },
    {
      label: "confidence ≥ 85%",
      pass: i.confidence >= 0.85,
    },
    {
      label: "ETA inside guardrail",
      pass: Number.isNaN(i.etaAlarmMin) ? undefined : i.etaAlarmMin <= 10,
    },
    { label: "cooldown / circuit breaker", pass: i.decision !== "HOLD" },
  ];

  return (
    <div className="grid gap-4 lg:grid-cols-[2fr_1fr]">
      <div className="space-y-4">
        <div className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="font-mono text-lg font-semibold">{i.deviceId}</h1>
            <span className="font-mono text-sm text-[#94a3b8]">{i.faultClass}</span>
            <span className="rounded bg-white/10 px-2 py-0.5 font-mono text-[11px]">
              confidence {(i.confidence * 100).toFixed(0)}%
            </span>
            {!Number.isNaN(i.etaAlarmMin) && (
              <span className="rounded bg-white/10 px-2 py-0.5 font-mono text-[11px]">
                ETA alarm {i.etaAlarmMin.toFixed(0)} min
              </span>
            )}
          </div>
          <p className="mt-2 text-sm">{i.reason}</p>
          {i.traceId && (
            <a
              href={datadogTraceURL(i.traceId)}
              target="_blank"
              rel="noreferrer"
              className="mt-2 inline-flex items-center gap-1 text-[12px] text-[#38bdf8] hover:underline"
            >
              <ArrowSquareOut className="h-3.5 w-3.5" aria-hidden /> decision trace in Datadog
            </a>
          )}
        </div>

        <RcaPanel incidentID={incidentID} initial={i.rca ?? null} />

        <div className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
          <h2 className="mb-2 text-sm font-semibold">Lifecycle</h2>
          <div className="flex flex-wrap gap-2">
            {!i.acknowledged && (
              <button
                onClick={() => act(() => api.ackIncident(incidentID))}
                className="cursor-pointer rounded border border-[#475569] px-3 py-1.5 text-sm hover:bg-white/10"
              >
                Acknowledge
              </button>
            )}
            {i.status === "OPEN" && (
              <button
                onClick={() => act(() => api.resolveIncident(incidentID))}
                className="cursor-pointer rounded bg-[#22c55e] px-3 py-1.5 text-sm font-semibold text-[#0f172a] hover:opacity-90"
              >
                Resolve
              </button>
            )}
            {i.status === "RESOLVED" && (
              <button
                onClick={() => act(() => api.feedbackIncident(incidentID, i.faultClass))}
                className="cursor-pointer rounded border border-[#475569] px-3 py-1.5 text-sm hover:bg-white/10"
              >
                Send feedback (retrain)
              </button>
            )}
          </div>
          {i.remediatedBy && (
            <p className="mt-2 text-[12px] text-[#94a3b8]">
              remediated by <span className="font-mono text-[#22c55e]">{i.remediatedBy}</span> at{" "}
              {i.remediatedAt && new Date(i.remediatedAt).toLocaleTimeString()}
            </p>
          )}
        </div>
      </div>

      <div className="space-y-4">
        <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
          <h2 className="mb-2 text-sm font-semibold">Guardrail gates</h2>
          <ul className="space-y-1 text-[12px]">
            {gates.map((g) => (
              <li key={g.label} className="flex items-center justify-between gap-2">
                <span>{g.label}</span>
                <span
                  className={
                    g.pass === undefined
                      ? "text-[#94a3b8]"
                      : g.pass
                        ? "text-[#22c55e]"
                        : "text-[#ef4444]"
                  }
                >
                  {g.pass === undefined ? "n/a" : g.pass ? "pass" : "fail"}
                </span>
              </li>
            ))}
          </ul>
        </section>

        <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
          <h2 className="mb-2 text-sm font-semibold">Why the AI decided this</h2>
          {(i.topFactors ?? []).length === 0 ? (
            <p className="text-[12px] text-[#94a3b8]">
              no factor attribution stored for this incident
            </p>
          ) : (
            <ul className="space-y-1.5">
              {(i.topFactors ?? []).map((f: TopFactor) => {
                const max = Math.max(
                  ...(i.topFactors ?? []).map((x) => Math.abs(x.contribution)),
                  1e-9,
                );
                const pct = Math.round((Math.abs(f.contribution) / max) * 100);
                const raises = f.direction === "raises";
                return (
                  <li key={f.feature} className="text-[12px]">
                    <div className="flex items-center justify-between font-mono text-[11px]">
                      <span>{f.feature}</span>
                      <span className={raises ? "text-[#ef4444]" : "text-[#38bdf8]"}>
                        z {f.z >= 0 ? "+" : ""}
                        {f.z.toFixed(2)} · {raises ? "+" : "−"}
                        {Math.abs(f.contribution).toFixed(2)}
                      </span>
                    </div>
                    <div className="mt-0.5 h-1.5 overflow-hidden rounded bg-white/5">
                      <div
                        className={`h-full rounded ${raises ? "bg-[#ef4444]" : "bg-[#38bdf8]"}`}
                        style={{ width: `${pct}%` }}
                      />
                    </div>
                  </li>
                );
              })}
            </ul>
          )}
          <p className="mt-2 text-[11px] text-[#94a3b8]">
            Top features by signed contribution (weight × z) to the winning class logit.
          </p>
        </section>

        <section className="rounded-lg border border-[#475569]/50 bg-[#1b2336] p-4">
          <h2 className="mb-2 text-sm font-semibold">Similar past incidents</h2>
          <ul className="space-y-1 font-mono text-[12px]">
            {(i.similar ?? []).length === 0 && (
              <li className="text-[#94a3b8]">vector memory empty (first incident)</li>
            )}
            {(i.similar ?? []).map((s, k) => (
              <li key={k} className="flex justify-between gap-2">
                <span>{s.deviceId}</span>
                <span className="text-[#94a3b8]">{s.faultClass}</span>
                <span className="text-[#38bdf8]">{(s.score * 100).toFixed(0)}%</span>
              </li>
            ))}
          </ul>
        </section>
      </div>
    </div>
  );
}
