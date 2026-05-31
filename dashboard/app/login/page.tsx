"use client";

// Login + onboarding entry point.
//
// One page, multiple modes selected by URL hash so a reload preserves
// state and we can link people to /login#forgot or /login#request:
//
//   (none / #signin)   email + password (+ TOTP challenge if needed)
//   #request           "request access" form (calls /auth/request-account)
//   #forgot            "forgot password" — email-only form
//   #reset             "I have a code" — code + new password
//   #verify            new-account "verify email" — code + first password
//
// Two-column on desktop (brand left, form right), stacks on mobile.

import { Suspense, useEffect, useState, FormEvent } from "react";
import { useRouter, useSearchParams } from "next/navigation";
import { api, getToken, setToken } from "@/lib/api";
import { Input } from "@/components/ui/input";
import { ThemeToggle } from "@/components/theme-toggle";
import { Database, ArrowRight, KeyRound, Mail, ShieldCheck } from "lucide-react";

type Mode = "signin" | "request" | "forgot" | "reset" | "verify";

// Next 15 requires useSearchParams() be inside a Suspense boundary for
// the static export to succeed. We render a minimal fallback so the
// brand pane and toggle still show during hydration.
export default function LoginPage() {
  return (
    <Suspense fallback={<div className="min-h-screen bg-bg" />}>
      <LoginPageInner />
    </Suspense>
  );
}

function LoginPageInner() {
  const router = useRouter();
  const params = useSearchParams();
  const [mode, setMode] = useState<Mode>("signin");
  const [flash, setFlash] = useState<{ kind: "ok" | "err"; msg: string } | null>(null);

  // Bail out if a token already exists in this tab — another tab probably
  // signed in. Same effect listens for storage-event changes so if a peer
  // tab signs in WHILE we're sitting on this page, we bounce immediately
  // instead of letting them submit a stale form.
  useEffect(() => {
    if (getToken()) {
      router.replace("/dashboard");
      return;
    }
    const onStorage = (e: StorageEvent) => {
      if (e.key === "ps3_token" && e.newValue) {
        router.replace("/dashboard");
      }
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, [router]);

  useEffect(() => {
    const sync = () => {
      const h = window.location.hash.replace(/^#/, "") as Mode;
      setMode((["signin", "request", "forgot", "reset", "verify"] as Mode[]).includes(h) ? h : "signin");
    };
    sync();
    window.addEventListener("hashchange", sync);
    return () => window.removeEventListener("hashchange", sync);
  }, []);

  const presetEmail = params.get("email") || "";

  const go = (m: Mode) => {
    window.location.hash = m === "signin" ? "" : m;
    setFlash(null);
  };

  return (
    <div className="min-h-screen grid lg:grid-cols-[minmax(0,1fr)_minmax(0,1.05fr)]">
      {/* Brand pane — hidden on mobile */}
      <aside className="hidden lg:flex flex-col justify-between bg-surface border-r border-border p-12 xl:p-16">
        <div className="flex items-center gap-2 text-sm font-medium">
          <Database size={16} className="text-link" />
          PersonalS3
        </div>
        <div className="max-w-md space-y-6">
          <p className="text-xs uppercase tracking-[0.18em] text-muted">Self-hosted storage</p>
          <h1 className="text-4xl xl:text-5xl font-semibold leading-[1.05] tracking-tight">
            Your own S3.
            <br />
            <span className="text-muted">Quiet, owned, accountable.</span>
          </h1>
          <p className="text-text-soft leading-relaxed">
            One place for your files, videos, and backups —
            controlled by you, accessed from the dashboard, the CLI,
            or any S3-compatible client.
          </p>
        </div>
        <p className="text-xs text-muted leading-relaxed">
          By signing in you agree this instance is operated by its administrator.
        </p>
      </aside>

      {/* Form pane */}
      <main className="relative flex items-center justify-center p-6 sm:p-10 lg:p-12">
        <div className="absolute top-4 right-4"><ThemeToggle /></div>

        <div className="w-full max-w-md">
          <div className="lg:hidden mb-8 flex items-center gap-2 text-sm font-medium">
            <Database size={16} className="text-link" />
            PersonalS3
          </div>

          {flash && (
            <div className={
              "mb-6 px-4 py-3 rounded-lg text-sm border " +
              (flash.kind === "ok"
                ? "bg-success/5 border-success/30 text-success"
                : "bg-danger/5 border-danger/30 text-danger")
            }>
              {flash.msg}
            </div>
          )}

          {mode === "signin"  && <SignInForm onFlash={setFlash} onSwitch={go} />}
          {mode === "request" && <RequestForm onFlash={setFlash} onSwitch={go} />}
          {mode === "forgot"  && <ForgotForm onFlash={setFlash} onSwitch={go} />}
          {mode === "reset"   && <CodeForm email={presetEmail} onFlash={setFlash} onSwitch={go} kind="reset"  />}
          {mode === "verify"  && <CodeForm email={presetEmail} onFlash={setFlash} onSwitch={go} kind="verify" />}
        </div>
      </main>
    </div>
  );
}

// ----------------------------------------------------------------------------

type Flash = { kind: "ok" | "err"; msg: string } | null;

function FormHeader({ icon, title, subtitle }: {
  icon: React.ReactNode; title: string; subtitle: string;
}) {
  return (
    <div className="mb-8">
      <div className="inline-flex items-center justify-center w-10 h-10 rounded-full border border-border text-muted mb-4">
        {icon}
      </div>
      <h2 className="text-2xl font-semibold tracking-tight">{title}</h2>
      <p className="text-sm text-text-soft mt-1.5">{subtitle}</p>
    </div>
  );
}

function FieldLabel({ children, htmlFor }: { children: React.ReactNode; htmlFor: string }) {
  return <label htmlFor={htmlFor} className="block text-xs font-medium text-text mb-1.5">{children}</label>;
}

function PrimaryButton({ children, loading, disabled }: {
  children: React.ReactNode; loading?: boolean; disabled?: boolean;
}) {
  return (
    <button
      type="submit"
      disabled={loading || disabled}
      className="w-full h-11 rounded-lg bg-text text-bg text-sm font-medium hover:opacity-90 disabled:opacity-40 transition-opacity inline-flex items-center justify-center gap-1.5"
    >
      {loading ? "Working…" : <>{children} <ArrowRight size={14} /></>}
    </button>
  );
}

function TextLink({ children, onClick }: { children: React.ReactNode; onClick: () => void }) {
  return (
    <button type="button" onClick={onClick}
      className="text-text underline underline-offset-4 decoration-border hover:decoration-text/40">
      {children}
    </button>
  );
}

// ----------------------------------------------------------------------------
// SIGN IN
// ----------------------------------------------------------------------------

function SignInForm({ onFlash, onSwitch }: { onFlash: (f: Flash) => void; onSwitch: (m: Mode) => void }) {
  const router = useRouter();
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [challenge, setChallenge] = useState<string | null>(null);
  const [trustDevice, setTrustDevice] = useState(false);
  const [loading, setLoading] = useState(false);

  const submitPassword = async (e: FormEvent) => {
    e.preventDefault();
    onFlash(null); setLoading(true);
    try {
      // If a trusted-device token exists from a past 2FA login, send it
      // so the server can skip the 2FA prompt entirely.
      const headers: Record<string, string> = {};
      const dt = typeof window !== "undefined" ? localStorage.getItem("ps3_device") : null;
      if (dt) headers["X-Trusted-Device"] = dt;

      const r = await api<{ token?: string; require2fa?: boolean; challenge?: string }>(
        "/auth/login",
        { method: "POST", body: JSON.stringify({ email, password }), auth: false, headers },
      );
      if (r.require2fa && r.challenge) { setChallenge(r.challenge); return; }
      if (r.token) { setToken(r.token); router.replace("/dashboard"); }
    } catch (e: unknown) {
      // Wrong cached device token? Drop it so the next attempt retries
      // cleanly with the 2FA challenge.
      if (typeof window !== "undefined") localStorage.removeItem("ps3_device");
      onFlash({ kind: "err", msg: e instanceof Error ? e.message : "Sign in failed" });
    } finally { setLoading(false); }
  };

  const submit2FA = async (e: FormEvent) => {
    e.preventDefault();
    if (!challenge) return;
    onFlash(null); setLoading(true);
    try {
      const url = trustDevice ? "/auth/2fa/login?trust=1" : "/auth/2fa/login";
      const r = await api<{ token: string; trustedDevice?: string }>(url, {
        method: "POST",
        body: JSON.stringify({ challenge, code: code.trim() }),
        auth: false,
      });
      setToken(r.token);
      if (r.trustedDevice && typeof window !== "undefined") {
        localStorage.setItem("ps3_device", r.trustedDevice);
      }
      router.replace("/dashboard");
    } catch (e: unknown) {
      onFlash({ kind: "err", msg: e instanceof Error ? e.message : "Invalid code" });
    } finally { setLoading(false); }
  };

  if (challenge) {
    return (
      <>
        <FormHeader icon={<ShieldCheck size={18} />} title="Two-factor code"
          subtitle="Enter the 6-digit code from your authenticator app." />
        <form onSubmit={submit2FA} className="space-y-5">
          <div>
            <FieldLabel htmlFor="otp">Code</FieldLabel>
            <Input id="otp" value={code} onChange={(e) => setCode(e.target.value)}
              autoFocus placeholder="000000"
              className="h-11 text-center text-lg tracking-[0.5em] font-mono" maxLength={10} />
          </div>
          <label className="flex items-center gap-2 text-sm text-text-soft cursor-pointer select-none">
            <input type="checkbox" checked={trustDevice}
              onChange={(e) => setTrustDevice(e.target.checked)}
              className="w-4 h-4 accent-text" />
            Trust this device for 30 days
            <span className="text-xs text-muted ml-1">(skip this prompt next time — max 3)</span>
          </label>
          <PrimaryButton loading={loading}>Verify</PrimaryButton>
        </form>
      </>
    );
  }

  return (
    <>
      <FormHeader icon={<KeyRound size={18} />} title="Sign in"
        subtitle="Welcome back. Use your email and password." />
      <form onSubmit={submitPassword} className="space-y-5">
        <div>
          <FieldLabel htmlFor="email">Email</FieldLabel>
          <Input id="email" type="email" value={email} onChange={(e) => setEmail(e.target.value)}
            autoComplete="email" placeholder="you@example.com" className="h-11" autoFocus />
        </div>
        <div>
          <div className="flex items-center justify-between mb-1.5">
            <FieldLabel htmlFor="password">Password</FieldLabel>
            <TextLink onClick={() => onSwitch("forgot")}>Forgot?</TextLink>
          </div>
          <Input id="password" type="password" value={password} onChange={(e) => setPassword(e.target.value)}
            autoComplete="current-password" className="h-11" />
        </div>
        <PrimaryButton loading={loading}>Continue</PrimaryButton>
      </form>
      <p className="mt-8 text-sm text-text-soft text-center">
        Don&apos;t have an account?{" "}
        <TextLink onClick={() => onSwitch("request")}>Request access</TextLink>
      </p>
      <p className="mt-2 text-sm text-text-soft text-center">
        Got a verification code?{" "}
        <TextLink onClick={() => onSwitch("verify")}>Set your password</TextLink>
      </p>
    </>
  );
}

// ----------------------------------------------------------------------------
// REQUEST ACCESS
// ----------------------------------------------------------------------------

function RequestForm({ onFlash, onSwitch }: { onFlash: (f: Flash) => void; onSwitch: (m: Mode) => void }) {
  const [email, setEmail] = useState("");
  const [name, setName] = useState("");
  const [reason, setReason] = useState("");
  const [loading, setLoading] = useState(false);
  const [done, setDone] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    onFlash(null); setLoading(true);
    try {
      await api("/auth/request-account", {
        method: "POST", auth: false,
        body: JSON.stringify({ email, name, reason }),
      });
      setDone(true);
    } catch (e: unknown) {
      onFlash({ kind: "err", msg: e instanceof Error ? e.message : "Couldn't submit" });
    } finally { setLoading(false); }
  };

  if (done) {
    return (
      <>
        <FormHeader icon={<Mail size={18} />} title="Request received"
          subtitle="The administrator has been notified. You'll get an email when they decide — usually within a day or two." />
        <button onClick={() => onSwitch("signin")}
          className="text-sm text-text-soft hover:text-text underline underline-offset-4 decoration-border">
          Back to sign in
        </button>
      </>
    );
  }

  return (
    <>
      <FormHeader icon={<Mail size={18} />} title="Request access"
        subtitle="Tell us who you are. New accounts start with 100 MB free; ask for more later once you're in." />
      <form onSubmit={submit} className="space-y-5">
        <div>
          <FieldLabel htmlFor="r-email">Email</FieldLabel>
          <Input id="r-email" type="email" required value={email}
            onChange={(e) => setEmail(e.target.value)} placeholder="you@example.com" className="h-11" autoFocus />
        </div>
        <div>
          <FieldLabel htmlFor="r-name">Your name</FieldLabel>
          <Input id="r-name" required value={name} onChange={(e) => setName(e.target.value)}
            placeholder="Pat Doe" className="h-11" />
        </div>
        <div>
          <FieldLabel htmlFor="r-reason">Why do you need access? (optional)</FieldLabel>
          <textarea id="r-reason" rows={3}
            value={reason} onChange={(e) => setReason(e.target.value)}
            placeholder="Personal photo archive, family backups, etc."
            className="w-full bg-bg border border-border rounded-md px-3 py-2 text-sm text-text placeholder:text-muted focus:border-text/40 focus:outline-none" />
        </div>
        <PrimaryButton loading={loading}>Submit request</PrimaryButton>
      </form>
      <p className="mt-8 text-sm text-text-soft text-center">
        Already have an account?{" "}
        <TextLink onClick={() => onSwitch("signin")}>Sign in</TextLink>
      </p>
    </>
  );
}

// ----------------------------------------------------------------------------
// FORGOT
// ----------------------------------------------------------------------------

function ForgotForm({ onFlash, onSwitch }: { onFlash: (f: Flash) => void; onSwitch: (m: Mode) => void }) {
  const [email, setEmail] = useState("");
  const [loading, setLoading] = useState(false);
  const [sent, setSent] = useState(false);

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    onFlash(null); setLoading(true);
    try {
      await api("/auth/forgot-password", {
        method: "POST", auth: false, body: JSON.stringify({ email }),
      });
      setSent(true);
    } catch (e: unknown) {
      onFlash({ kind: "err", msg: e instanceof Error ? e.message : "Couldn't send" });
    } finally { setLoading(false); }
  };

  if (sent) {
    return (
      <>
        <FormHeader icon={<Mail size={18} />} title="Check your email"
          subtitle="If your address is on file we sent a one-time code. It expires in 10 minutes." />
        <button onClick={() => { window.location.hash = "reset"; }}
          className="w-full h-11 rounded-lg border border-border text-sm font-medium hover:border-text/40 transition-colors">
          I have the code →
        </button>
        <p className="mt-4 text-xs text-muted text-center">
          Didn&apos;t arrive? Check spam, then{" "}
          <TextLink onClick={() => onSwitch("signin")}>contact your administrator</TextLink>.
        </p>
      </>
    );
  }

  return (
    <>
      <FormHeader icon={<Mail size={18} />} title="Reset your password"
        subtitle="We'll email a one-time code you can use to set a new password." />
      <form onSubmit={submit} className="space-y-5">
        <div>
          <FieldLabel htmlFor="f-email">Email</FieldLabel>
          <Input id="f-email" type="email" required value={email}
            onChange={(e) => setEmail(e.target.value)} placeholder="you@example.com" className="h-11" autoFocus />
        </div>
        <PrimaryButton loading={loading}>Email me a code</PrimaryButton>
      </form>
      <p className="mt-8 text-sm text-text-soft text-center">
        Remembered it?{" "}
        <TextLink onClick={() => onSwitch("signin")}>Sign in</TextLink>
      </p>
    </>
  );
}

// ----------------------------------------------------------------------------
// CODE (reset + verify share this shape)
// ----------------------------------------------------------------------------

function CodeForm({
  email: presetEmail, onFlash, onSwitch, kind,
}: {
  email: string;
  onFlash: (f: Flash) => void;
  onSwitch: (m: Mode) => void;
  kind: "reset" | "verify";
}) {
  const [email, setEmail] = useState(presetEmail);
  const [code, setCode] = useState("");
  const [password, setPassword] = useState("");
  const [loading, setLoading] = useState(false);
  const [done, setDone] = useState(false);

  const endpoint = kind === "reset" ? "/auth/reset-password" : "/auth/verify-email";
  const title    = kind === "reset" ? "Set a new password" : "Verify your email";
  const subtitle = kind === "reset"
    ? "Enter the 6-digit code we emailed you, plus a new password (8+ characters)."
    : "Enter the code from your approval email and pick a password.";

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    onFlash(null); setLoading(true);
    try {
      await api(endpoint, {
        method: "POST", auth: false,
        body: JSON.stringify({ email, code: code.trim(), password }),
      });
      setDone(true);
    } catch (e: unknown) {
      onFlash({ kind: "err", msg: e instanceof Error ? e.message : "Failed" });
    } finally { setLoading(false); }
  };

  if (done) {
    return (
      <>
        <FormHeader icon={<ShieldCheck size={18} />} title="All set"
          subtitle={kind === "reset" ? "Your password has been updated." : "Your account is verified."} />
        <button onClick={() => onSwitch("signin")}
          className="w-full h-11 rounded-lg bg-text text-bg text-sm font-medium hover:opacity-90">
          Sign in
        </button>
      </>
    );
  }

  return (
    <>
      <FormHeader icon={<KeyRound size={18} />} title={title} subtitle={subtitle} />
      <form onSubmit={submit} className="space-y-5">
        <div>
          <FieldLabel htmlFor="c-email">Email</FieldLabel>
          <Input id="c-email" type="email" required value={email}
            onChange={(e) => setEmail(e.target.value)} className="h-11" />
        </div>
        <div>
          <FieldLabel htmlFor="c-code">One-time code</FieldLabel>
          <Input id="c-code" required value={code} onChange={(e) => setCode(e.target.value)}
            placeholder="000000" maxLength={10}
            className="h-11 text-center text-lg tracking-[0.5em] font-mono" />
        </div>
        <div>
          <FieldLabel htmlFor="c-pw">{kind === "reset" ? "New password" : "Choose a password"}</FieldLabel>
          <Input id="c-pw" type="password" required minLength={8}
            value={password} onChange={(e) => setPassword(e.target.value)} className="h-11" />
        </div>
        <PrimaryButton loading={loading}>{kind === "reset" ? "Update password" : "Activate account"}</PrimaryButton>
      </form>
      <p className="mt-8 text-sm text-text-soft text-center">
        Back to <TextLink onClick={() => onSwitch("signin")}>sign in</TextLink>
      </p>
    </>
  );
}
