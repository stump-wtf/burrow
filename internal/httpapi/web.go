package httpapi

// Burrow's design language: "ink on paper", taking OpenWork's own values so
// the two read as one family (ink #011627, cool slate grays, hairline borders,
// soft ink-tinted shadows, 150ms transitions, 2px ink focus rings — and no
// accent colors anywhere).
//
// Governing: docs/design-language.md (tokens, components, rules). The token
// block below is the source of truth; docs/design-language.md documents it.
// New web surfaces (the /admin console) embed pageCSS rather than restyling
// from scratch.
const pageCSS = `:root{
  --ink:#011627;          /* text, primary buttons, focus rings */
  --body:#374151;         /* body copy */
  --muted:#6b7684;        /* secondary copy, meta */
  --faint:#8a93a0;        /* placeholders, disabled */
  --border:#e8edf3;       /* hairlines */
  --surface:#f7f8fa;      /* code blocks, wells */
  --hover:#f5f7fa;        /* hover wash */
  --page:#fcfcfc;         /* page background */
  --card:#ffffff;
  --btn-fg:#ffffff;       /* text on the primary button */
  --btn-hover:#000000;    /* OpenWork's own primary-button hover */
  --card-shadow:0 8px 24px rgba(1,22,39,.05);
  --radius-card:16px;
  --radius-control:8px;
}
@media (prefers-color-scheme:dark){:root{
  --ink:#f5f7fa;
  --body:#c3ccd6;
  --muted:#8a93a0;
  --faint:#5c6b7a;
  --border:#1d2a3a;
  --surface:#101b29;
  --hover:#152238;
  --page:#0b1220;
  --card:#0e1826;
  --btn-fg:#0b1220;
  --btn-hover:#dbe2ea;
  --card-shadow:0 8px 24px rgba(0,0,0,.35);
}}
*{box-sizing:border-box}
html,body{margin:0}
body{
  min-height:100svh;display:grid;place-items:center;
  padding:24px;background:var(--page);color:var(--ink);
  font:15px/1.6 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
  -webkit-font-smoothing:antialiased;
}
.shell{display:flex;flex-direction:column;align-items:center;gap:16px;width:100%}
.wordmark{
  font-size:14px;font-weight:600;letter-spacing:-.01em;
  color:var(--muted);user-select:none;
}
.card{
  width:min(100%,400px);padding:28px;background:var(--card);
  border:1px solid var(--border);border-radius:var(--radius-card);
  box-shadow:var(--card-shadow);
}
.card h1{margin:0;font-size:19px;font-weight:600;letter-spacing:-.015em}
.note{margin:12px 0 0;color:var(--body)}
.note a{color:var(--ink)}
/* the one-time grant (the contract requires showing it as copyable text) */
#grant{
  display:block;margin-top:14px;padding:10px 12px;
  background:var(--surface);border:1px solid var(--border);
  border-radius:var(--radius-control);
  font:12.5px/1.5 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;
  color:var(--ink);word-break:break-all;user-select:all;
}
.button{
  display:block;width:100%;margin-top:18px;padding:10px 16px;
  background:var(--ink);color:var(--btn-fg);
  border:none;border-radius:var(--radius-control);
  font-size:14px;font-weight:500;text-align:center;text-decoration:none;
  cursor:pointer;transition:background-color .15s ease;
}
.button:hover{background:var(--btn-hover)}
.button:focus-visible{outline:2px solid var(--ink);outline-offset:2px}
.alt{margin:12px 0 0;font-size:13px;color:var(--muted)}
.alt a{color:var(--muted);word-break:break-all}
.alt a:hover{color:var(--ink)}
@media (prefers-reduced-motion:reduce){.button{transition:none}}`
