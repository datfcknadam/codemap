// codemap UI: three zoom levels drawn as graphs (directory scope, package)
// and one as a neighbourhood view (function or type). Routing lives in the
// URL hash so browser back/forward walks the zoom history.
"use strict";

const $ = (s) => document.querySelector(s);
const stage = $("#stage");
const panel = $("#panel");
const opts = { tests: false, generated: false };
let cy = null;

try { cytoscape.use(cytoscapeDagre); } catch (_) { /* already registered */ }

// ---- persistence of toggles (per-viewer convenience only) ----
for (const key of Object.keys(opts)) {
  try { opts[key] = localStorage.getItem("codemap." + key) === "1"; } catch (_) {}
  const box = $("#" + key);
  box.checked = opts[key];
  box.addEventListener("change", () => {
    opts[key] = box.checked;
    try { localStorage.setItem("codemap." + key, box.checked ? "1" : "0"); } catch (_) {}
    route();
  });
}

// ---- helpers ----
const css = (name) => getComputedStyle(document.documentElement).getPropertyValue(name).trim();
const esc = (s) => String(s ?? "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
const trunc = (s, n = 96) => (s.length > n ? s.slice(0, n - 1) + "…" : s);
const enc = encodeURIComponent;
const href = (kind, id) => `#/${kind}/${enc(id)}`;

async function api(path, params = {}) {
  const q = new URLSearchParams({ ...params, tests: opts.tests ? "1" : "0", generated: opts.generated ? "1" : "0" });
  const t0 = performance.now();
  const res = await fetch(`${path}?${q}`);
  const body = await res.json();
  $("#status").textContent = `${Math.round(performance.now() - t0)} мс`;
  if (!res.ok) throw new Error(body.error || res.statusText);
  return body;
}

function crumbs(parts) {
  // parts: [{label, href?}] — the last one is the current level.
  $("#crumbs").innerHTML = parts
    .map((p, i) => (i === parts.length - 1 || !p.href ? `<span class="here">${esc(p.label)}</span>` : `<a href="${p.href}">${esc(p.label)}</a>`))
    .join('<span class="sep">/</span>');
}

function pathCrumbs(module, rel) {
  const parts = [{ label: module || "module", href: href("scope", "") }];
  let acc = "";
  for (const seg of rel ? rel.split("/") : []) {
    acc = acc ? acc + "/" + seg : seg;
    parts.push({ label: seg, href: href("scope", acc) });
  }
  return parts;
}

function showError(err) {
  if (cy) { cy.destroy(); cy = null; }
  stage.innerHTML = `<div class="error">${esc(err.message || err)}</div>`;
}

// ---- routing ----
let module = "";

async function route() {
  const [, kind = "scope", ...rest] = location.hash.slice(1).split("/");
  const id = decodeURIComponent(rest.join("/"));
  try {
    if (kind === "pkg") await renderPackage(id);
    else if (kind === "node") await renderNode(id);
    else await renderScope(id);
  } catch (err) {
    showError(err);
  }
}
window.addEventListener("hashchange", route);

// ---- graph drawing ----
const measure = (() => {
  const ctx = document.createElement("canvas").getContext("2d");
  return (lines, font) => {
    ctx.font = font;
    const w = Math.max(...lines.map((l) => ctx.measureText(l).width));
    return { w: Math.ceil(w) + 24, h: lines.length * 15 + 16 };
  };
})();

function nodeEl(id, lines, classes, data = {}) {
  const font = `11px ${css("--mono")}`;
  const { w, h } = measure(lines, font);
  return { group: "nodes", data: { id, label: lines.join("\n"), w, h, ...data }, classes };
}

function draw(elements, { onTap, onHover }) {
  if (cy) cy.destroy();
  stage.innerHTML = "";
  const box = document.createElement("div");
  box.style.cssText = "position:absolute;inset:0";
  stage.appendChild(box);
  if (!elements.some((e) => e.group === "nodes")) {
    stage.innerHTML = '<div class="empty">Здесь пусто. Включите «тесты» или «сгенерированное», если ищете их.</div>';
    return;
  }
  cy = cytoscape({
    container: box,
    elements,
    wheelSensitivity: 0.25,
    minZoom: 0.1,
    maxZoom: 2.5,
    style: [
      { selector: "node", style: {
        shape: "round-rectangle", width: "data(w)", height: "data(h)",
        "background-color": css("--card"), "border-width": 1, "border-color": css("--card-border"),
        label: "data(label)", color: css("--text"), "font-family": css("--mono"), "font-size": 11,
        "text-wrap": "wrap", "text-valign": "center", "text-halign": "center", "text-justification": "left",
      } },
      { selector: "node.dir", style: { "background-color": css("--dir"), "border-color": css("--dir-border") } },
      { selector: "node.self", style: { "border-width": 2, "border-color": css("--accent") } },
      { selector: "node.interface", style: { "border-color": css("--iface"), "border-width": 1.5 } },
      { selector: "node.ext", style: { "background-color": css("--ext"), "border-style": "dashed", color: css("--muted") } },
      { selector: "node.test", style: { "border-style": "dashed", opacity: 0.8 } },
      { selector: "node.unexported", style: { color: css("--muted") } },
      { selector: "edge", style: {
        width: "mapData(weight, 1, 30, 1, 6)", "line-color": css("--edge"), "target-arrow-color": css("--edge"),
        "target-arrow-shape": "triangle", "arrow-scale": 0.8, "curve-style": "bezier", opacity: 0.75,
      } },
      { selector: "edge.interface", style: { "line-style": "dashed", "line-color": css("--iface"), "target-arrow-color": css("--iface") } },
      { selector: "edge.async", style: { "line-style": "dotted", width: 2.5, "line-color": css("--async"), "target-arrow-color": css("--async") } },
      { selector: "edge.labelled", style: {
        label: "data(via)", "font-size": 10, color: css("--async"), "text-rotation": "autorotate",
        "text-background-color": css("--bg"), "text-background-opacity": 1, "text-background-padding": 2,
      } },
      { selector: ".faded", style: { opacity: 0.12 } },
      { selector: "node.hl", style: { "border-width": 2, "border-color": css("--accent") } },
    ],
    layout: { name: "dagre", rankDir: "LR", nodeSep: 22, rankSep: 80, edgeSep: 8, padding: 30, fit: true },
  });

  cy.on("mouseover", "node", (e) => {
    const n = e.target;
    const near = n.closedNeighborhood();
    cy.elements().not(near).addClass("faded");
    n.addClass("hl");
    near.edges(".async").addClass("labelled");
    onHover && onHover(n.data());
  });
  cy.on("mouseout", "node", () => cy.elements().removeClass("faded hl labelled"));
  cy.on("mouseover", "edge", (e) => e.target.addClass("labelled"));
  cy.on("mouseout", "edge", (e) => e.target.removeClass("labelled"));
  cy.on("tap", "node", (e) => onTap(e.target.data()));
  // Fit small graphs; for big ones keep text readable and start at the
  // busiest node instead of shrinking everything into illegible dots.
  cy.ready(() => {
    if (cy.zoom() > 1.2) { cy.zoom(1.2); cy.center(); }
    if (cy.zoom() < 0.7) {
      const hub = cy.nodes().max((n) => n.degree()).ele;
      cy.zoom(0.7);
      cy.center(hub);
      $("#status").textContent = "колесо — масштаб, перетаскивание — сдвиг";
    }
  });
}

// ---- level 1: directory scope ----
async function renderScope(prefix) {
  const data = await api("/api/scope", { prefix });
  module = data.module;
  crumbs(pathCrumbs(data.module, prefix));
  const els = data.nodes.map((n) => {
    const self = n.id === prefix && n.kind === "pkg";
    const name = self ? `● ${n.label} (этот пакет)` : n.label + (n.kind === "dir" ? "/" : "");
    const stats = n.kind === "dir"
      ? `${n.packages} пак. · ${n.types} типов · ${n.funcs} функций`
      : `${n.types} типов · ${n.funcs} функций`;
    const lines = [name, stats];
    if (n.doc) lines.push(trunc(n.doc, 60));
    return nodeEl("s:" + n.id, lines, [n.kind, self ? "self" : ""].join(" "), { raw: n });
  });
  for (const e of data.edges) {
    els.push({ group: "edges", data: { id: `${e.from}>${e.to}`, source: "s:" + e.from, target: "s:" + e.to, weight: e.weight } });
  }
  draw(els, {
    onTap: (d) => {
      const n = d.raw;
      location.hash = n.kind === "dir" ? href("scope", n.id) : href("pkg", n.package);
    },
    onHover: (d) => {
      const n = d.raw;
      panel.innerHTML = `
        <span class="kind">${n.kind === "dir" ? "папка" : "пакет"}</span>
        <h4>${esc(n.id || data.module)}</h4>
        ${n.doc ? `<div class="doc">${esc(n.doc)}</div>` : ""}
        <ul>
          ${n.kind === "dir" ? `<li>${n.packages} пакетов</li>` : ""}
          <li>${n.types} типов, ${n.funcs} функций</li>
          <li>${n.outside} импортов за пределы «${esc(prefix || data.module)}»</li>
          ${n.generated ? "<li>содержит сгенерированный код</li>" : ""}
        </ul>
        <p class="hint">${n.kind === "dir" ? "Клик — открыть папку." : "Клик — открыть пакет."}</p>`;
    },
  });
  panel.innerHTML = `<h4>${esc(prefix || data.module)}</h4>
    <p class="hint">Папки и пакеты этого уровня; стрелки — импорты между ними.${data.hidden ? ` Скрыто сгенерированных пакетов: ${data.hidden}.` : ""}</p>
    ${data.warnings?.length ? `<div class="section">Ошибки загрузки</div><ul>${data.warnings.map((w) => `<li>${esc(w)}</li>`).join("")}</ul>` : ""}`;
}

// ---- level 2: package ----
const stripRecv = (sig) => sig.replace(/^func \([^)]*\) /, "");

function cardLines(c) {
  const lines = [trunc(c.signature)];
  if (c.kind === "type" && c.fields?.length) {
    for (const f of c.fields.slice(0, 6)) lines.push("  " + trunc(f, 70));
    if (c.fields.length > 6) lines.push(`  … ещё ${c.fields.length - 6} полей`);
  }
  if (c.members?.length) {
    if (c.kind === "type") lines.push("  ──");
    const max = c.kind === "interface" ? 10 : 8;
    for (const m of c.members.slice(0, max)) lines.push("  " + trunc(stripRecv(m.signature), 90));
    if (c.members.length > max) lines.push(`  … ещё ${c.members.length - max} методов`);
  }
  return lines;
}

async function renderPackage(id) {
  const data = await api("/api/package", { id });
  crumbs([...pathCrumbs(module, data.rel.split("/").slice(0, -1).join("/")), { label: data.name }]);
  const els = data.cards.map((c) => nodeEl(c.id, cardLines(c),
    [c.kind, c.test ? "test" : "", c.exported ? "" : "unexported"].join(" "), { card: c }));
  for (const x of data.externals) {
    els.push(nodeEl(x.id, ["→ " + x.label], "ext", { ext: x }));
  }
  for (const e of data.edges) {
    els.push({ group: "edges", classes: e.kind, data: {
      id: `${e.from}>${e.to}>${e.kind}>${e.via || ""}`, source: e.from, target: e.to, weight: e.weight, via: e.via || "",
    } });
  }
  const pkgPanel = () => `
    <span class="kind">пакет</span><h4>${esc(data.rel || data.name)}</h4>
    ${data.doc ? `<div class="doc">${esc(data.doc)}</div>` : ""}
    <div class="section">Импортирует</div><ul>${data.imports.map((i) => `<li><a href="${href("pkg", i)}">${esc(i)}</a></li>`).join("") || "<li>—</li>"}</ul>
    <div class="section">Импортируют его</div><ul>${data.importedBy.map((i) => `<li><a href="${href("pkg", i)}">${esc(i)}</a></li>`).join("") || "<li>—</li>"}</ul>`;
  draw(els, {
    onTap: (d) => { location.hash = d.ext ? href("pkg", d.ext.id) : href("node", d.card.id); },
    onHover: (d) => {
      if (d.ext) {
        panel.innerHTML = `<span class="kind">другой пакет</span><h4>${esc(d.ext.label)}</h4><p class="hint">Вызовы из этого пакета туда. Клик — открыть.</p>`;
        return;
      }
      const c = d.card;
      panel.innerHTML = `
        <span class="kind">${esc(c.kind)}${c.test ? " · тест" : ""}</span>
        <h4>${esc(c.signature)}</h4>
        <div class="loc">${esc(c.loc)}</div>
        ${c.doc ? `<div class="doc">${esc(c.doc)}</div>` : ""}
        ${c.fields?.length ? `<div class="section">Поля</div><ul>${c.fields.map((f) => `<li>${esc(f)}</li>`).join("")}</ul>` : ""}
        ${c.members?.length ? `<div class="section">Методы</div><ul>${c.members.map((m) => `<li><a href="${href("node", m.id)}">${esc(stripRecv(m.signature))}</a></li>`).join("")}</ul>` : ""}
        <p class="hint">Клик — открыть реализацию и соседей.</p>`;
    },
  });
  panel.innerHTML = pkgPanel();
}

// ---- level 3: function or type with its neighbourhood ----
const ARROW = { "": "→", interface: "⇢" };

// splitID separates "path/to/pkg.Type.Method" into package path and the rest.
function splitID(id) {
  const dot = id.indexOf(".", id.lastIndexOf("/") + 1);
  return dot < 0 ? [id, ""] : [id.slice(0, dot), id.slice(dot + 1)];
}

// compact names a symbol relative to the package being viewed:
// "Manager.postSend" at home, "port.Worker.ProcessSend" elsewhere.
function compact(id, home) {
  const [pkg, local] = splitID(id);
  if (!local) return id;
  return pkg === home ? local : pkg.split("/").pop() + "." + local;
}

function refHTML(r, home) {
  const cls = r.via === "interface" ? "interface" : r.via ? "async" : "";
  const arrow = ARROW[r.via] ?? "⇝";
  // Async labels carry module-relative IDs; re-root them before compacting.
  const via = cls === "async" ? r.via.replace(/^(\S+) (.*)$/, (_, k, id) => `${k} ${compact(module + "/" + id, home)}`) : "";
  return `<a class="ref ${cls} ${r.test ? "test" : ""}" href="${href("node", r.id)}" title="${esc(r.short)}">
    <div class="name"><span class="arrow">${arrow}</span>${esc(compact(r.id, home))}</div>
    ${r.signature ? `<div class="sig">${esc(trunc(r.signature, 140))}</div>` : ""}
    ${via ? `<div class="via">через ${esc(via)}</div>` : ""}
  </a>`;
}

function column(title, refs, home, extra = "") {
  return `<section class="col"><h3>${title} (${refs.length})</h3>${refs.map((r) => refHTML(r, home)).join("") || '<div class="ext">—</div>'}${extra}</section>`;
}

async function renderNode(id) {
  const n = await api("/api/node", { id });
  if (cy) { cy.destroy(); cy = null; }
  const rel = n.package.startsWith(module + "/") ? n.package.slice(module.length + 1) : "";
  const parts = [...pathCrumbs(module, rel.split("/").slice(0, -1).join("/")), { label: n.package.split("/").pop(), href: href("pkg", n.package) }];
  if (n.parent) parts.push({ label: n.parent.short.split(".").pop(), href: href("node", n.parent.id) });
  parts.push({ label: n.name });
  crumbs(parts);

  const isType = n.kind === "type" || n.kind === "interface";
  const members = n.members?.length && isType
    ? `<div class="section">Методы</div><ul class="members">${n.members.map((m) => `<li><a href="${href("node", m.id)}">${esc(stripRecv(m.signature))}</a></li>`).join("")}</ul>` : "";
  const fields = n.fields?.length
    ? `<div class="section">${n.kind === "interface" ? "Встраивает" : "Поля"}</div><ul class="members">${n.fields.map((f) => `<li>${esc(f)}</li>`).join("")}</ul>` : "";
  const external = n.external.length
    ? `<details class="ext"><summary>внешние вызовы (${n.external.length})</summary>${n.external.map((e) => `<div>${esc(e)}</div>`).join("")}</details>` : "";

  const home = n.package;
  const left = isType ? column("Реализует", n.implements, home) : column("Кто вызывает", n.callers, home);
  const right = isType ? column("Реализуют", n.implementedBy, home) : column("Что вызывает", n.callees, home, external);
  stage.innerHTML = `<div class="ego">
    ${left}
    <section class="focus">
      <span class="badge">${esc(n.kind)}</span><span class="loc">${esc(n.loc)}</span>
      <div class="title">${highlight(n.signature)}</div>
      ${n.doc ? `<div class="doc">${esc(n.doc)}</div>` : ""}
      ${fields}${members}
      ${n.source ? `<pre class="code">${highlight(n.source)}</pre>` : ""}
    </section>
    ${right}
  </div>`;
  stage.scrollTop = 0;
  panel.innerHTML = `<span class="kind">${esc(n.kind)}</span><h4>${esc(n.short)}</h4>
    <div class="loc">${esc(n.loc)}</div>
    <p class="hint">Слева — откуда сюда приходит управление, справа — куда уходит.
    ⇢ — через интерфейс, ⇝ — через канал, шину или колбэк.</p>`;
}

// ---- minimal Go highlighter ----
const GO_KW = new Set(("break case chan const continue default defer else fallthrough for func go goto if import " +
  "interface map package range return select struct switch type var nil true false iota").split(" "));
const TOKEN = /(\/\/[^\n]*|\/\*[\s\S]*?\*\/)|("(?:\\.|[^"\\\n])*"|`[^`]*`|'(?:\\.|[^'\\\n])*')|\b(\d[\d_.xXa-fA-F]*)\b|\b([A-Za-z_]\w*)\b/g;

function highlight(code) {
  let out = "", last = 0;
  for (const m of code.matchAll(TOKEN)) {
    out += esc(code.slice(last, m.index));
    const [tok, com, str, num, word] = m;
    if (com) out += `<span class="tok-com">${esc(com)}</span>`;
    else if (str) out += `<span class="tok-str">${esc(str)}</span>`;
    else if (num) out += `<span class="tok-num">${esc(num)}</span>`;
    else if (GO_KW.has(word)) out += `<span class="tok-kw">${word}</span>`;
    else out += esc(tok);
    last = m.index + tok.length;
  }
  return out + esc(code.slice(last));
}

// ---- search ----
const q = $("#q");
const hitsEl = $("#hits");
let hits = [], active = 0, timer = 0;

function openHit(h) {
  hitsEl.hidden = true;
  q.blur();
  location.hash = h.kind === "package" ? href("pkg", h.id) : href("node", h.id);
}

function renderHits() {
  hitsEl.hidden = hits.length === 0;
  hitsEl.innerHTML = hits.map((h, i) => `<li class="${i === active ? "active" : ""}" data-i="${i}">
    <div class="name">${esc(h.short)}</div><div class="sig">${esc(h.kind)} · ${esc(h.signature || "")}</div></li>`).join("");
}

q.addEventListener("input", () => {
  clearTimeout(timer);
  timer = setTimeout(async () => {
    try { hits = await api("/api/search", { q: q.value }); } catch (_) { hits = []; }
    active = 0;
    renderHits();
  }, 120);
});
q.addEventListener("keydown", (e) => {
  if (e.key === "ArrowDown") { active = Math.min(active + 1, hits.length - 1); renderHits(); e.preventDefault(); }
  else if (e.key === "ArrowUp") { active = Math.max(active - 1, 0); renderHits(); e.preventDefault(); }
  else if (e.key === "Enter" && hits[active]) openHit(hits[active]);
  else if (e.key === "Escape") { hitsEl.hidden = true; q.blur(); }
});
hitsEl.addEventListener("mousedown", (e) => {
  const li = e.target.closest("li");
  if (li) openHit(hits[+li.dataset.i]);
});
q.addEventListener("blur", () => setTimeout(() => { hitsEl.hidden = true; }, 150));
document.addEventListener("keydown", (e) => {
  if (e.key === "/" && document.activeElement !== q) { e.preventDefault(); q.focus(); q.select(); }
});

// Redraw graphs in the right palette when the system theme flips.
matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => { if (cy) route(); });

// The scope endpoint also tells us the module path used for breadcrumbs.
(async () => {
  try { module = (await api("/api/scope", { prefix: "__none__" })).module; } catch (_) {}
  route();
})();
