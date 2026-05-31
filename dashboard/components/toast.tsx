"use client";

import { useState, useEffect, useCallback, createContext, useContext } from "react";
import { CheckCircle2, XCircle, Info, X } from "lucide-react";

type Kind = "success" | "error" | "info";

interface Toast {
  id: string;
  kind: Kind;
  message: string;
  // Auto-dismiss after this many ms (0 = sticky)
  ttl: number;
}

interface ToastCtx {
  push: (kind: Kind, message: string, ttl?: number) => void;
}

const Ctx = createContext<ToastCtx>({ push: () => {} });

/** useToast() — call from anywhere inside <ToastHost>. */
export function useToast() { return useContext(Ctx); }

/**
 * Renders toasts bottom-right. Wrap once near the app root.
 */
export function ToastHost({ children }: { children: React.ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);

  const push = useCallback((kind: Kind, message: string, ttl = 4000) => {
    const id = crypto.randomUUID();
    setToasts((prev) => [...prev, { id, kind, message, ttl }]);
    if (ttl > 0) {
      setTimeout(() => setToasts((prev) => prev.filter((t) => t.id !== id)), ttl);
    }
  }, []);

  const dismiss = (id: string) => setToasts((prev) => prev.filter((t) => t.id !== id));

  return (
    <Ctx.Provider value={{ push }}>
      {children}
      <div className="fixed bottom-4 right-4 z-50 flex flex-col gap-2 max-w-sm">
        {toasts.map((t) => <ToastBox key={t.id} t={t} onDismiss={() => dismiss(t.id)} />)}
      </div>
    </Ctx.Provider>
  );
}

function ToastBox({ t, onDismiss }: { t: Toast; onDismiss: () => void }) {
  // Slide-in animation on mount
  const [shown, setShown] = useState(false);
  useEffect(() => { setShown(true); }, []);

  const Icon =
    t.kind === "success" ? CheckCircle2 :
    t.kind === "error"   ? XCircle      : Info;
  const accent =
    t.kind === "success" ? "text-success border-success/40" :
    t.kind === "error"   ? "text-danger border-danger/40"   :
                           "text-accent border-accent/40";

  return (
    <div
      className={`
        flex items-start gap-2 px-3 py-2 bg-panel border rounded-lg shadow-lg
        text-sm transition-all duration-200
        ${accent}
        ${shown ? "translate-x-0 opacity-100" : "translate-x-4 opacity-0"}
      `}
    >
      <Icon size={16} className="shrink-0 mt-0.5" />
      <span className="flex-1 text-text">{t.message}</span>
      <button onClick={onDismiss} className="text-muted hover:text-text shrink-0">
        <X size={14} />
      </button>
    </div>
  );
}
