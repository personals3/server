"use client";

import { useEffect, useState } from "react";
import { useRouter } from "next/navigation";
import { api, ApiError } from "@/lib/api";
import { Card } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { DocsLink } from "@/components/ui/docs-link";
import { PageHeader } from "@/components/ui/page-header";
import { SectionHeader } from "@/components/ui/section";
import { Shield, ShieldCheck, KeyRound, AlertTriangle, Copy, Check } from "lucide-react";
import { useToast } from "@/components/toast";

interface Me {
  email: string;
  role: string;
  totpEnabled: boolean;
}

export default function SecurityPage() {
  const toast = useToast();
  const [me, setMe] = useState<Me | null>(null);
  const [enabled, setEnabled] = useState<boolean | null>(null);
  const [setupData, setSetupData] = useState<{
    secret: string;
    otpauthUrl: string;
    recoveryCodes: string[];
  } | null>(null);
  const [verifyCode, setVerifyCode] = useState("");
  const [disableCode, setDisableCode] = useState("");
  const [regenCode, setRegenCode] = useState("");
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);

  const refresh = async () => {
    try {
      const me = await api<Me>("/auth/me");
      setMe(me);
      setEnabled(me.totpEnabled);
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    }
  };

  useEffect(() => { void refresh(); }, []);

  const startSetup = async () => {
    setBusy(true);
    try {
      const r = await api<{
        secret: string;
        otpauthUrl: string;
        recoveryCodes: string[];
      }>("/auth/2fa/setup", { method: "POST" });
      setSetupData(r);
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const verify = async () => {
    setBusy(true);
    try {
      await api("/auth/2fa/verify", {
        method: "POST",
        body: JSON.stringify({ code: verifyCode.trim() }),
      });
      toast.push("success", "2FA enabled. Save your recovery codes — they won't be shown again.");
      // Refresh user state from server so UI reflects new totpEnabled.
      await refresh();
      // Keep setupData visible so the user can still copy recovery codes.
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "wrong code");
    } finally {
      setBusy(false);
    }
  };

  const regenerate = async () => {
    if (!confirm("Generate new recovery codes? All existing codes will be invalidated.")) return;
    setBusy(true);
    try {
      const r = await api<{ recoveryCodes: string[] }>(
        "/auth/2fa/recovery/regen",
        {
          method: "POST",
          body: JSON.stringify({ code: regenCode.trim() }),
        },
      );
      // Reuse the setupData slot so the new codes get shown the same way.
      setSetupData({
        secret: "",
        otpauthUrl: "",
        recoveryCodes: r.recoveryCodes,
      });
      setRegenCode("");
      toast.push("success", "New recovery codes — save them now.");
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "failed");
    } finally {
      setBusy(false);
    }
  };

  const disable = async () => {
    if (!confirm("Disable 2FA? Account becomes password-only again.")) return;
    setBusy(true);
    try {
      await api("/auth/2fa/disable", {
        method: "POST",
        body: JSON.stringify({ code: disableCode.trim() }),
      });
      toast.push("success", "2FA disabled.");
      setDisableCode("");
      setSetupData(null);
      await refresh();
    } catch (e) {
      toast.push("error", e instanceof ApiError ? e.message : "wrong code");
    } finally {
      setBusy(false);
    }
  };

  const copyCodes = () => {
    if (!setupData) return;
    navigator.clipboard.writeText(setupData.recoveryCodes.join("\n"));
    setCopied(true);
    setTimeout(() => setCopied(false), 1500);
  };

  return (
    <div className="max-w-2xl">
      <PageHeader
        title="Security"
        description={
          <>
            Protect your account with two-factor authentication, manage
            trusted devices, and change your password.{" "}
            <DocsLink slug="account/logging-in">Sign-in &amp; 2FA guide</DocsLink>
          </>
        }
      />

      <div className="space-y-6">
      <Card>
        <SectionHeader
          title={
            <span className="inline-flex items-center gap-2">
              {enabled
                ? <ShieldCheck size={14} className="text-success" />
                : <Shield size={14} className="text-muted" />}
              Two-factor authentication (TOTP)
            </span>
          }
          description={
            enabled
              ? "Enabled. You'll need a code from your authenticator app on every login."
              : "Disabled. A single password is the only thing protecting your account."
          }
          actions={
            !enabled && !setupData ? (
              <Button onClick={startSetup} disabled={busy}>Set up 2FA</Button>
            ) : null
          }
        />
        {setupData && !enabled && (
          <div className="mt-4 space-y-4 border-t border-border pt-4">
            <div>
              <p className="text-xs text-muted mb-2">
                1. Scan this QR code with Google Authenticator, Authy, 1Password, etc.
              </p>
              <div className="bg-white p-3 rounded inline-block">
                <img
                  alt="2FA QR code"
                  src={`https://api.qrserver.com/v1/create-qr-code/?size=200x200&data=${encodeURIComponent(setupData.otpauthUrl)}`}
                />
              </div>
              <details className="mt-2 text-xs">
                <summary className="cursor-pointer text-muted">Can't scan? Enter manually</summary>
                <code className="block mt-1 text-[11px] bg-bg p-2 rounded font-mono break-all">
                  {setupData.secret}
                </code>
              </details>
            </div>

            <div className="bg-amber-500/10 border border-amber-500/40 rounded p-3">
              <div className="flex items-start gap-2">
                <KeyRound size={14} className="text-amber-400 shrink-0 mt-0.5" />
                <div className="flex-1 min-w-0">
                  <p className="text-xs font-semibold text-amber-200">Save these recovery codes</p>
                  <p className="text-[11px] text-amber-300/80 mb-2">
                    Shown once. Each works as a one-time backup code if you lose your authenticator.
                  </p>
                  <div className="font-mono text-[11px] grid grid-cols-2 gap-1">
                    {setupData.recoveryCodes.map((c) => (
                      <code key={c} className="bg-amber-950/40 px-2 py-1 rounded">{c}</code>
                    ))}
                  </div>
                  <button
                    onClick={copyCodes}
                    className="mt-2 text-[11px] text-amber-300 inline-flex items-center gap-1 hover:underline"
                  >
                    {copied ? <Check size={11} /> : <Copy size={11} />}
                    {copied ? "Copied" : "Copy all"}
                  </button>
                </div>
              </div>
            </div>

            <div>
              <p className="text-xs text-muted mb-1">
                2. Enter a code from your authenticator to enable:
              </p>
              <div className="flex items-center gap-2">
                <Input
                  type="text"
                  value={verifyCode}
                  onChange={(e) => setVerifyCode(e.target.value)}
                  placeholder="6-digit code"
                  maxLength={6}
                  className="font-mono w-32"
                  autoFocus
                />
                <Button onClick={verify} disabled={busy || verifyCode.length < 6}>
                  Enable
                </Button>
              </div>
            </div>
          </div>
        )}

        {enabled && (
          <div className="mt-4 border-t border-border pt-4 space-y-4">
            {/* Confirmation banner */}
            <div className="bg-green-500/10 border border-green-500/40 rounded p-3 flex items-start gap-2">
              <ShieldCheck size={14} className="text-green-400 shrink-0 mt-0.5" />
              <div className="text-xs">
                <p className="font-semibold text-green-300">2FA is active on this account.</p>
                <p className="text-green-300/80 mt-0.5">
                  Future logins require a 6-digit code from your authenticator app
                  (or a recovery code).
                </p>
              </div>
            </div>

            {/* Regenerate recovery codes */}
            <div>
              <h3 className="text-xs font-semibold inline-flex items-center gap-1">
                <KeyRound size={12} /> Regenerate recovery codes
              </h3>
              <p className="text-[11px] text-muted mt-0.5">
                Lost your codes or used most of them? Generate a new set.
                All existing codes are invalidated.
              </p>
              <div className="flex items-center gap-2 mt-2">
                <Input
                  type="text"
                  value={regenCode}
                  onChange={(e) => setRegenCode(e.target.value)}
                  placeholder="current code"
                  className="font-mono w-48"
                />
                <Button onClick={regenerate} disabled={busy || regenCode.length < 6}
                        variant="secondary">
                  Generate new codes
                </Button>
              </div>
            </div>

            {/* Disable 2FA */}
            <div>
              <h3 className="text-xs font-semibold inline-flex items-center gap-1 text-danger">
                <AlertTriangle size={12} /> Disable 2FA
              </h3>
              <p className="text-[11px] text-muted mt-0.5">
                Removes the secret + recovery codes. Account drops back to
                password-only. Re-enable any time from this page.
              </p>
              <div className="flex items-center gap-2 mt-2">
                <Input
                  type="text"
                  value={disableCode}
                  onChange={(e) => setDisableCode(e.target.value)}
                  placeholder="current code"
                  className="font-mono w-48"
                />
                <Button onClick={disable} disabled={busy || disableCode.length < 6}
                        className="bg-danger hover:bg-red-600">
                  Disable
                </Button>
              </div>
            </div>
          </div>
        )}
      </Card>

      <TrustedDevicesCard />
      <DangerZoneCard />
      </div>
    </div>
  );
}

// ============================================================================
// Trusted devices card — list, revoke. Tokens are minted by 2FA login.
// ============================================================================

interface TrustedDevice {
  id: string;
  label: string;
  ipAddress: string;
  userAgent: string;
  lastUsedAt: string;
  expiresAt: string;
  createdAt: string;
}

function TrustedDevicesCard() {
  const toast = useToast();
  const [devices, setDevices] = useState<TrustedDevice[]>([]);
  const [max, setMax] = useState(3);
  const [loaded, setLoaded] = useState(false);

  const load = () => api<{ devices: TrustedDevice[]; max: number }>("/auth/me/devices")
    .then((r) => { setDevices(r.devices || []); setMax(r.max); setLoaded(true); })
    .catch(() => setLoaded(true));

  useEffect(() => { void load(); }, []);

  const revoke = async (id: string) => {
    if (!confirm("Revoke this device? You'll need to pass 2FA again on next sign-in from there.")) return;
    try {
      await api(`/auth/me/devices/${id}`, { method: "DELETE" });
      toast.push("success", "Device revoked.");
      await load();
    } catch (e) {
      toast.push("error", e instanceof Error ? e.message : "revoke failed");
    }
  };

  if (!loaded) return null;

  return (
    <Card>
      <div className="flex items-baseline justify-between mb-3">
        <h2 className="text-sm font-semibold">Trusted devices</h2>
        <span className="text-xs text-muted">{devices.length} of {max}</span>
      </div>
      <p className="text-xs text-text-soft mb-4">
        When you sign in with 2FA, you can ask the server to remember this
        device for 30 days — skips the 2FA prompt next time. Limit {max} devices;
        the oldest is dropped if you add another.
      </p>
      {devices.length === 0 ? (
        <p className="text-sm text-muted italic">No trusted devices. Each successful 2FA sign-in is offered the chance to trust the device.</p>
      ) : (
        <ul className="divide-y divide-border">
          {devices.map((d) => (
            <li key={d.id} className="py-3 flex flex-wrap items-start justify-between gap-3">
              <div className="min-w-0">
                <p className="text-sm font-medium">{d.label || "Unnamed device"}</p>
                <p className="text-xs text-muted font-mono break-all">{d.userAgent}</p>
                <p className="text-xs text-muted mt-1">
                  IP {d.ipAddress || "—"} · last used {new Date(d.lastUsedAt).toLocaleString()} · expires {new Date(d.expiresAt).toLocaleDateString()}
                </p>
              </div>
              <button onClick={() => revoke(d.id)}
                className="text-xs text-danger hover:underline shrink-0">
                Revoke
              </button>
            </li>
          ))}
        </ul>
      )}
    </Card>
  );
}

// ============================================================================
// Danger zone — self-delete with OTP confirmation.
// ============================================================================

function DangerZoneCard() {
  const router = useRouter();
  const toast = useToast();
  const [step, setStep] = useState<"idle" | "code" | "deleting">("idle");
  const [code, setCode] = useState("");
  const [busy, setBusy] = useState(false);

  const requestOTP = async () => {
    setBusy(true);
    try {
      await api("/auth/me/delete-request", { method: "POST", body: "{}" });
      setStep("code");
      toast.push("success", "We emailed you a one-time code. It expires in 10 minutes.");
    } catch (e) {
      toast.push("error", e instanceof Error ? e.message : "request failed");
    } finally { setBusy(false); }
  };

  const confirmDelete = async () => {
    if (!confirm("Last chance — this permanently deletes your account, every bucket, every file, every share link. There is no recovery. Proceed?")) return;
    setBusy(true); setStep("deleting");
    try {
      await api("/auth/me", { method: "DELETE", body: JSON.stringify({ code: code.trim() }) });
      // Logged out → bounce home.
      localStorage.removeItem("ps3_token");
      router.replace("/?deleted=1");
    } catch (e) {
      setStep("code");
      toast.push("error", e instanceof Error ? e.message : "delete failed");
    } finally { setBusy(false); }
  };

  return (
    <Card className="border-danger/40">
      <h2 className="text-sm font-semibold text-danger mb-1">Danger zone</h2>
      <p className="text-xs text-text-soft mb-4">
        Permanently delete your account. Every bucket, file, version, share link,
        and API key is wiped. <strong>No recovery, no grace period, no trash.</strong>
      </p>
      {step === "idle" && (
        <button onClick={requestOTP} disabled={busy}
          className="px-3 py-1.5 rounded-md border border-danger/40 text-danger text-sm font-medium hover:bg-danger/5 disabled:opacity-40">
          {busy ? "Sending code…" : "Delete my account"}
        </button>
      )}
      {step === "code" && (
        <div className="space-y-3 max-w-md">
          <p className="text-sm text-text-soft">
            We emailed a 6-digit code. Enter it below to confirm.
          </p>
          <Input value={code} onChange={(e) => setCode(e.target.value)}
            placeholder="000000" maxLength={10}
            className="h-11 text-center text-lg tracking-[0.5em] font-mono" />
          <div className="flex gap-2 flex-wrap">
            <button onClick={confirmDelete} disabled={busy || code.trim().length < 4}
              className="px-3 py-1.5 rounded-md bg-danger text-white text-sm font-medium hover:opacity-90 disabled:opacity-40">
              Permanently delete account
            </button>
            <button onClick={() => { setStep("idle"); setCode(""); }}
              className="px-3 py-1.5 text-sm text-text-soft hover:text-text">
              Cancel
            </button>
          </div>
        </div>
      )}
      {step === "deleting" && <p className="text-sm text-muted">Deleting…</p>}
    </Card>
  );
}
