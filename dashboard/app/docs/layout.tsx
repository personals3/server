// Public docs layout. No auth gate, no sidebar — just a minimal
// page wrapper. The actual reader chrome lives in the docs page itself.

export default function PublicDocsLayout({ children }: { children: React.ReactNode }) {
  return <>{children}</>;
}
