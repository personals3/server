import { ReactNode } from "react";
import { ExternalLink } from "lucide-react";
import { cn } from "@/lib/format";

// The user-docs site. Env-overridable so forks/self-hosters can point at
// their own docs (or hide the links by serving their own site).
export const DOCS_URL = (
  process.env.NEXT_PUBLIC_DOCS_URL || "https://developers.personals3.tech"
).replace(/\/$/, "");

interface DocsLinkProps {
  /** Docs page slug, e.g. "account/api-keys" */
  slug: string;
  children?: ReactNode;
  className?: string;
}

/**
 * Contextual "learn more" link into the docs site — always a new tab.
 * Drop into PageHeader/SectionHeader descriptions or inline prose so
 * users can read up on a concept without losing their place.
 */
export function DocsLink({ slug, children = "Learn more", className }: DocsLinkProps) {
  return (
    <a
      href={`${DOCS_URL}/${slug}/`}
      target="_blank"
      rel="noopener noreferrer"
      // Usable inside clickable surfaces (upload dropzone, list rows)
      // without triggering their handlers.
      onClick={(e) => e.stopPropagation()}
      className={cn(
        "inline-flex items-baseline gap-0.5 text-accent hover:underline whitespace-nowrap",
        className,
      )}
    >
      {children}
      <ExternalLink size={11} className="self-center" aria-hidden />
    </a>
  );
}
