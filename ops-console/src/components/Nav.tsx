"use client";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useLiveInvalidation } from "@/lib/live";

const links = [
  { href: "/", label: "Fleet" },
  { href: "/incidents", label: "Incidents" },
  { href: "/ai", label: "AI Control" },
  { href: "/events", label: "Events" },
  { href: "/compliance", label: "Compliance" },
];

export function Nav() {
  const path = usePathname();
  const connected = useLiveInvalidation();
  return (
    <header className="glass sticky top-0 z-20 flex items-center gap-6 border-b px-4 py-2">
      <Link href="/" className="font-mono text-sm font-bold tracking-tight">
        ColdTrace <span className="text-[#22c55e]">Ops</span>
      </Link>
      <nav aria-label="Primary" className="flex gap-1">
        {links.map((l) => (
          <Link
            key={l.href}
            href={l.href}
            className={`rounded px-2.5 py-1 text-sm transition-colors hover:bg-white/10 ${
              path === l.href ? "bg-white/10 font-medium" : "text-[#94a3b8]"
            }`}
          >
            {l.label}
          </Link>
        ))}
      </nav>
      <div className="ml-auto flex items-center gap-2 text-[11px] font-mono text-[#94a3b8]">
        <span
          className={`inline-block h-2 w-2 rounded-full ${connected ? "bg-[#22c55e]" : "bg-[#ef4444]"}`}
          aria-hidden
        />
        {connected ? "live" : "reconnecting…"}
      </div>
    </header>
  );
}
