// Minimal markdown → HTML renderer with anchored headings + ToC extraction.
// Renders into HTML that pairs with the .prose styles in globals.css.

export interface MarkdownResult {
  html: string;
  toc: TocEntry[];
}

export interface TocEntry {
  level: 2 | 3 | 4;
  id: string;
  text: string;
}

function escapeHTML(s: string): string {
  return s.replace(/&/g, "&amp;")
          .replace(/</g, "&lt;")
          .replace(/>/g, "&gt;");
}

// turn "Per-rung quota publish" → "per-rung-quota-publish"
function slugify(s: string): string {
  return s.toLowerCase()
          .replace(/[`*_~]/g, "")
          .replace(/[^\w\s-]/g, "")
          .trim()
          .replace(/\s+/g, "-")
          .slice(0, 80);
}

function inline(s: string): string {
  let t = escapeHTML(s);
  // links [text](url) before code so a link inside a code span doesn't bind
  t = t.replace(/\[([^\]]+)\]\(([^)]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
  // inline images ![alt](url) — must come before bold to avoid * conflict
  t = t.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, '<img src="$2" alt="$1" loading="lazy" />');
  // inline code
  t = t.replace(/`([^`]+)`/g, "<code>$1</code>");
  // bold
  t = t.replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>");
  t = t.replace(/__([^_]+)__/g,    "<strong>$1</strong>");
  // italic
  t = t.replace(/(^|[^*])\*([^*\n]+)\*/g, "$1<em>$2</em>");
  return t;
}

export function renderMarkdown(src: string): MarkdownResult {
  const lines = src.split("\n");
  const out: string[] = [];
  const toc: TocEntry[] = [];
  const seenSlugs = new Map<string, number>();

  let i = 0;
  let inFence = false;
  let fenceLang = "";
  let fenceBuf: string[] = [];
  let listStack: ("ul" | "ol")[] = [];
  let paraBuf: string[] = [];

  const flushPara = () => {
    if (paraBuf.length === 0) return;
    out.push(`<p>${inline(paraBuf.join(" "))}</p>`);
    paraBuf = [];
  };
  const flushLists = () => {
    while (listStack.length > 0) out.push(`</${listStack.pop()}>`);
  };
  const uniqueSlug = (base: string) => {
    const n = seenSlugs.get(base) || 0;
    seenSlugs.set(base, n + 1);
    return n === 0 ? base : `${base}-${n}`;
  };

  while (i < lines.length) {
    const raw = lines[i];
    const ln = raw.replace(/\s+$/, "");

    const fence = /^```(\w*)\s*$/.exec(ln);
    if (fence) {
      if (inFence) {
        out.push(`<pre><code class="language-${fenceLang}">${escapeHTML(fenceBuf.join("\n"))}</code></pre>`);
        inFence = false;
        fenceBuf = [];
        fenceLang = "";
      } else {
        flushPara(); flushLists();
        inFence = true;
        fenceLang = fence[1] || "";
      }
      i++; continue;
    }
    if (inFence) { fenceBuf.push(raw); i++; continue; }

    if (ln === "") { flushPara(); flushLists(); i++; continue; }

    const h = /^(#{1,6})\s+(.+)$/.exec(ln);
    if (h) {
      flushPara(); flushLists();
      const level = h[1].length;
      const text = h[2];
      const slug = uniqueSlug(slugify(text));
      if (level === 2 || level === 3 || level === 4) {
        toc.push({ level: level as 2 | 3 | 4, id: slug, text });
      }
      const anchor = `<a href="#${slug}" class="anchor" aria-label="link">#</a>`;
      out.push(`<h${level} id="${slug}">${inline(text)}${level > 1 ? anchor : ""}</h${level}>`);
      i++; continue;
    }

    if (/^---+$/.test(ln)) { flushPara(); flushLists(); out.push("<hr />"); i++; continue; }

    if (ln.startsWith("|") && i + 1 < lines.length && /^\|[\s|:-]+\|$/.test(lines[i + 1].trim())) {
      flushPara(); flushLists();
      const header = ln.split("|").slice(1, -1).map((c) => c.trim());
      i += 2;
      const body: string[][] = [];
      while (i < lines.length && lines[i].trim().startsWith("|")) {
        body.push(lines[i].split("|").slice(1, -1).map((c) => c.trim()));
        i++;
      }
      out.push("<table><thead><tr>"
        + header.map((c) => `<th>${inline(c)}</th>`).join("")
        + "</tr></thead><tbody>"
        + body.map((row) => "<tr>" + row.map((c) => `<td>${inline(c)}</td>`).join("") + "</tr>").join("")
        + "</tbody></table>");
      continue;
    }

    // Standalone image ![](url) on its own line → block-level <figure>
    const img = /^!\[([^\]]*)\]\(([^)]+)\)\s*$/.exec(ln);
    if (img) {
      flushPara(); flushLists();
      out.push(`<figure><img src="${img[2]}" alt="${escapeHTML(img[1])}" loading="lazy" />${img[1] ? `<figcaption>${inline(img[1])}</figcaption>` : ""}</figure>`);
      i++; continue;
    }

    // Standalone video — markdown doesn't have native syntax, so we
    // recognize "@video[caption](url)" as a friendly extension.
    const vid = /^@video\[([^\]]*)\]\(([^)]+)\)\s*$/.exec(ln);
    if (vid) {
      flushPara(); flushLists();
      out.push(`<figure><video src="${vid[2]}" controls preload="metadata"></video>${vid[1] ? `<figcaption>${inline(vid[1])}</figcaption>` : ""}</figure>`);
      i++; continue;
    }

    const ul = /^(\s*)[-*]\s+(.+)$/.exec(ln);
    if (ul) {
      flushPara();
      if (listStack[listStack.length - 1] !== "ul") {
        flushLists(); listStack.push("ul"); out.push("<ul>");
      }
      out.push(`<li>${inline(ul[2])}</li>`);
      i++; continue;
    }
    const ol = /^(\s*)\d+\.\s+(.+)$/.exec(ln);
    if (ol) {
      flushPara();
      if (listStack[listStack.length - 1] !== "ol") {
        flushLists(); listStack.push("ol"); out.push("<ol>");
      }
      out.push(`<li>${inline(ol[2])}</li>`);
      i++; continue;
    }

    const bq = /^>\s?(.*)$/.exec(ln);
    if (bq) {
      flushPara(); flushLists();
      out.push(`<blockquote><p>${inline(bq[1])}</p></blockquote>`);
      i++; continue;
    }

    flushLists();
    paraBuf.push(ln);
    i++;
  }

  flushPara(); flushLists();
  if (inFence) {
    out.push(`<pre><code>${escapeHTML(fenceBuf.join("\n"))}</code></pre>`);
  }
  return { html: out.join("\n"), toc };
}
