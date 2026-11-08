"use client";
// Live state providers: TanStack Query + the SSE hub. SSE invalidates the
// relevant queries — the console never polls, it reacts.
import { useEffect, useRef, useState } from "react";
import { QueryClient, QueryClientProvider, useQueryClient } from "@tanstack/react-query";
import { openStream, type ChangeEvent } from "./api";

export function Providers({ children }: { children: React.ReactNode }) {
  const [client] = useState(
    () =>
      new QueryClient({
        defaultOptions: { queries: { refetchOnWindowFocus: false, staleTime: 5000 } },
      }),
  );
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/** Subscribes to the ops-api SSE hub and invalidates queries per source. */
export function useLiveInvalidation() {
  const qc = useQueryClient();
  const esRef = useRef<EventSource | null>(null);
  const [connected, setConnected] = useState(false);
  useEffect(() => {
    esRef.current = openStream((e: ChangeEvent) => {
      if (e.source === "device_state") qc.invalidateQueries({ queryKey: ["fleet"] });
      if (e.source === "ai_incidents") qc.invalidateQueries({ queryKey: ["incidents"] });
      if (e.source === "events") qc.invalidateQueries({ queryKey: ["events"] });
    });
    esRef.current.onopen = () => setConnected(true);
    esRef.current.onerror = () => setConnected(false);
    return () => esRef.current?.close();
  }, [qc]);
  return connected;
}
