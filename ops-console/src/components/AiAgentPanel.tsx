"use client";
// AI agent chat panel — streaming SSE chat with the ops-api AI agent.
// Ask things like "why is FZ-003 alarming?" — answers are grounded by the
// backend; the agent can only suggest actions, humans execute them.
import { useEffect, useRef, useState } from "react";
import { Sparkle, X } from "@phosphor-icons/react";
import { api } from "@/lib/api";

type Msg = { role: "user" | "assistant"; content: string };

export function AiAgentPanel() {
  const [open, setOpen] = useState(false);
  const [msgs, setMsgs] = useState<Msg[]>([]);
  const [input, setInput] = useState("");
  const [streaming, setStreaming] = useState(false);
  const bottom = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setOpen((o) => !o);
      }
      if (e.key === "Escape") setOpen(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  useEffect(() => {
    bottom.current?.scrollIntoView({ block: "end" });
  }, [msgs, streaming]);

  const send = async () => {
    const q = input.trim();
    if (!q || streaming) return;
    const next: Msg[] = [...msgs, { role: "user", content: q }];
    setMsgs(next);
    setInput("");
    setStreaming(true);
    const assistant: Msg = { role: "assistant", content: "" };
    setMsgs([...next, assistant]);
    try {
      await api.chatStream(next, (delta) => {
        assistant.content += delta;
        setMsgs([...next, { ...assistant }]);
      });
      if (!assistant.content) {
        assistant.content = "[no answer — check the AI agent engine config]";
        setMsgs([...next, { ...assistant }]);
      }
    } catch (e) {
      assistant.content += `\n[connection error: ${e instanceof Error ? e.message : "stream failed"}]`;
      setMsgs([...next, { ...assistant }]);
    } finally {
      setStreaming(false);
    }
  };

  return (
    <>
      <button
        onClick={() => setOpen(true)}
        className="glass fixed bottom-4 right-4 z-30 inline-flex cursor-pointer items-center gap-2 rounded-full px-4 py-2 text-sm font-medium shadow-lg transition-opacity hover:opacity-95"
        aria-label="Open AI agent chat"
      >
        <Sparkle weight="fill" className="h-4 w-4 text-[#38bdf8]" aria-hidden />
        AI agent
        <kbd className="rounded bg-white/10 px-1.5 py-0.5 font-mono text-[10px] text-[#94a3b8]">⌘K</kbd>
      </button>

      {open && (
        <aside
          role="dialog"
          aria-label="AI agent chat"
          className="glass fixed bottom-4 right-4 z-30 flex h-[520px] w-[380px] flex-col rounded-xl p-3"
        >
          <div className="mb-2 flex items-center justify-between">
            <span className="flex items-center gap-1.5 text-sm font-semibold">
              <Sparkle weight="fill" className="h-4 w-4 text-[#38bdf8]" aria-hidden /> AI agent
            </span>
            <button
              onClick={() => setOpen(false)}
              aria-label="Close chat"
              className="cursor-pointer rounded p-1 hover:bg-white/10"
            >
              <X className="h-4 w-4" aria-hidden />
            </button>
          </div>

          <div className="flex-1 space-y-2 overflow-y-auto pr-1">
            {msgs.length === 0 && (
              <div className="rounded-lg border border-[#475569]/40 p-3 text-[12px] leading-relaxed text-[#94a3b8]">
                Ask about the fleet:{" "}
                <em>&ldquo;why is FZ-003 alarming?&rdquo;</em>,{" "}
                <em>&ldquo;what did the AI do in the last hour?&rdquo;</em> — answers are
                grounded in MongoDB. The AI agent suggests actions; you execute them.
              </div>
            )}
            {msgs.map((m, i) => (
              <div
                key={i}
                className={`max-w-[90%] whitespace-pre-wrap rounded-lg px-3 py-2 text-[13px] leading-relaxed ${
                  m.role === "user"
                    ? "ml-auto bg-[#38bdf8]/20"
                    : "bg-[#272f42]"
                }`}
              >
                {m.content || (streaming && i === msgs.length - 1 ? "…" : "")}
              </div>
            ))}
            <div ref={bottom} />
          </div>

          <form
            onSubmit={(e) => {
              e.preventDefault();
              void send();
            }}
            className="mt-2 flex gap-2"
          >
            <input
              value={input}
              onChange={(e) => setInput(e.target.value)}
              placeholder={streaming ? "thinking…" : "ask the AI agent…"}
              className="flex-1 rounded border border-[#475569] bg-[#0f172a] px-3 py-2 text-[13px]"
              disabled={streaming}
            />
            <button
              type="submit"
              disabled={streaming || !input.trim()}
              className="cursor-pointer rounded bg-[#38bdf8] px-3 text-sm font-semibold text-[#0f172a] transition-opacity hover:opacity-90 disabled:opacity-40"
            >
              Send
            </button>
          </form>
        </aside>
      )}
    </>
  );
}
