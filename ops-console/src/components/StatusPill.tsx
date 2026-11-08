"use client";
// Status pill: color + label + icon (never color alone — a11y).
import { Warning, CheckCircle, WarningCircle, Timer } from "@phosphor-icons/react";

const CONF = {
  OK: { color: "bg-[#22c55e]/15 text-[#22c55e] border-[#22c55e]/40", icon: CheckCircle, label: "OK" },
  AT_RISK: { color: "bg-[#f59e0b]/15 text-[#f59e0b] border-[#f59e0b]/40", icon: Warning, label: "AT RISK" },
  ALARM: { color: "bg-[#ef4444]/15 text-[#ef4444] border-[#ef4444]/40", icon: WarningCircle, label: "ALARM" },
  STALE: { color: "bg-[#475569]/30 text-[#94a3b8] border-[#475569]/50", icon: Timer, label: "STALE" },
} as const;

export function StatusPill({ status }: { status: keyof typeof CONF }) {
  const c = CONF[status] ?? CONF.STALE;
  const Icon = c.icon;
  return (
    <span
      className={`inline-flex items-center gap-1 rounded border px-1.5 py-0.5 text-[11px] font-mono font-medium ${c.color}`}
    >
      <Icon weight="fill" aria-hidden className="h-3 w-3" />
      {c.label}
    </span>
  );
}
