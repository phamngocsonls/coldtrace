"use client";
// Incidents list + detail with the AI decision card, RCA and workflow actions.
import Link from "next/link";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Incident } from "@/lib/api";
import { RcaPanel } from "@/components/RcaPanel";

function DecisionBadge({ d }: { d: Incident["decision"] }) {
  const tone =
    d === "AUTO" ? "text-[#22c55e]" : d === "HOLD" ? "text-[#f59e0b]" : "text-[#38bdf8]";
  return <span className={`font-mono text-[11px] font-bold ${tone}`}>{d}</span>;
}

export default function IncidentsPage() {
  const incidents = useQuery({
    queryKey: ["incidents"],
    queryFn: () => api.incidents(),
    refetchInterval: 8000,
  });
  return (
    <div className="space-y-3">
      <h1 className="text-lg font-semibold">AI Incidents</h1>
      <ul className="space-y-2">
        {(incidents.data ?? []).map((i) => (
          <li key={i._id}>
            <Link
              href={`/incidents/${i._id}`}
              className="block rounded-lg border border-[#475569]/50 bg-[#1b2336] p-3 transition-colors hover:border-[#38bdf8]/60"
            >
              <div className="flex flex-wrap items-center gap-2 font-mono text-[13px]">
                <span className="font-semibold">{i.deviceId}</span>
                <span className="text-[#94a3b8]">{i.faultClass}</span>
                <span className="text-[#94a3b8]">conf {(i.confidence * 100).toFixed(0)}%</span>
                <DecisionBadge d={i.decision} />
                <span
                  className={
                    i.status === "OPEN"
                      ? "rounded bg-[#ef4444]/15 px-1.5 py-0.5 text-[10px] text-[#ef4444]"
                      : "rounded bg-[#22c55e]/15 px-1.5 py-0.5 text-[10px] text-[#22c55e]"
                  }
                >
                  {i.status}
                </span>
                <span className="ml-auto text-[11px] text-[#94a3b8]">
                  {new Date(i.openedAt).toLocaleTimeString()}
                </span>
              </div>
              <p className="mt-1 line-clamp-1 text-[12px] text-[#94a3b8]">{i.reason}</p>
            </Link>
          </li>
        ))}
        {!incidents.data?.length && (
          <li className="text-[#94a3b8]">no incidents recorded</li>
        )}
      </ul>
    </div>
  );
}
