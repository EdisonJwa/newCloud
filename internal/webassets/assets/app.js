// newcloud — browser client
//
// Deliberately dependency-free: no framework, no bundler, no npm tree. The whole
// application is three static files embedded in the binary, so the shipped artifact
// contains exactly the code that was reviewed, with no transitive dependencies to audit
// and no build step to reproduce.
//
// Two rules hold everywhere in this file:
//
//  1. User data reaches the DOM through textContent, never innerHTML. File names, share
//     labels and addresses come from other people; injecting one into markup is stored
//     XSS on the session of whoever browses the directory.
//  2. The server decides what may be previewed and thumbnailed. The client reads the
//     `preview` field the API returns; it never infers "this is an image" from an
//     extension and then disagrees with the server about it.
//
// The interaction model is the file-manager convention: single click selects and fills the
// inspector, double click (or Enter) opens. That is what removes the "click and get thrown
// somewhere" problem, and it is why the selection state below is real state rather than a
// transient CSS class.

(() => {
  "use strict";

  // ---------- state ----------

  const state = {
    me: null,
    view: "files", // files | shares | trash | audit | access
    path: "",
    entries: [],
    layout: "list", // list | grid
    filter: "",
    // Selection is a set of paths. A range needs the anchor to extend from.
    selected: new Set(),
    anchor: null,
    railCollapsed: false,
    inspectorClosed: false,
    // Cached detail for the inspected file: versions and the shares pointing at it.
    detail: { path: null, versions: null, shares: null, loading: false },
    audit: {
      events: [],
      offset: 0,
      total: 0,
      hasMore: false,
      actions: [],
      action: "",
      result: "",
      since: "",
      loading: false,
    },
  };

  const $ = (sel) => document.querySelector(sel);

  // ---------- DOM helper ----------

  function el(tag, opts = {}, children = []) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(opts)) {
      if (value == null || value === false) continue;
      if (key === "text") node.textContent = value;
      else if (key === "class") node.className = value;
      else if (key === "dataset") Object.assign(node.dataset, value);
      else if (key === "style") node.setAttribute("style", value);
      else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
      else node.setAttribute(key, value === true ? "" : value);
    }
    for (const child of [].concat(children)) {
      if (child == null || child === false) continue;
      node.append(typeof child === "string" ? document.createTextNode(child) : child);
    }
    return node;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
  }

  // icon returns an inline SVG. Icons are drawn here rather than loaded as a font or
  // sprite because the CSP allows no external resources and the set is small.
  function icon(name, size = 15) {
    const paths = {
      search: "M10.5 10.5l3.5 3.5M7.5 12.5a5 5 0 100-10 5 5 0 000 10z",
      files: "M3 4.5A1.5 1.5 0 014.5 3h2.4l1.2 1.5H13A1 1 0 0114 5.5v7.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4.5z",
      shares: "M6 9.5l4.5-3.5M6 7l4.5 3.5M4 9.5a1.75 1.75 0 110-3.5 1.75 1.75 0 010 3.5zm8 4.5a1.75 1.75 0 110-3.5 1.75 1.75 0 010 3.5zm0-8a1.75 1.75 0 110-3.5 1.75 1.75 0 010 3.5z",
      trash: "M3.5 5h9M6 5V3.8a.8.8 0 01.8-.8h2.4a.8.8 0 01.8.8V5M5 5l.4 8a.9.9 0 00.9.9h3.4a.9.9 0 00.9-.9L11 5",
      key: "M9.5 6.5a2.5 2.5 0 11-1.7 4.3L4.5 14H3v-1.5l3.8-3.3a2.5 2.5 0 012.7-2.7z",
      pulse: "M1.5 8h3l1.5-4 2.5 8 1.5-4h3.5",
      upload: "M8 11V3m0 0L5 6m3-3l3 3M2.5 11.5v1.5a1 1 0 001 1h9a1 1 0 001-1v-1.5",
      folderPlus: "M3 4.5A1.5 1.5 0 014.5 3h2.4l1.2 1.5H13A1 1 0 0114 5.5v7.5a1 1 0 01-1 1H4a1 1 0 01-1-1V4.5zM8 7.5v3.5M6.25 9.25h3.5",
      refresh: "M13.5 8a5.5 5.5 0 11-1.6-3.9M13.5 2v3h-3",
      list: "M2.5 4h11M2.5 8h11M2.5 12h11",
      grid: "M2.5 2.5h4.5v4.5H2.5zM9 2.5h4.5v4.5H9zM2.5 9h4.5v4.5H2.5zM9 9h4.5v4.5H9z",
      download: "M8 2.5v8m0 0l3-3m-3 3l-3-3M2.5 13.5h11",
      close: "M4 4l8 8M12 4l-8 8",
      doc: "M4 2.5h5l3 3v8H4zM9 2.5v3h3",
      user: "M8 8a2.5 2.5 0 100-5 2.5 2.5 0 000 5zm-4.5 5.5a4.5 4.5 0 019 0",
      sidebar: "M2.5 3.5h11v9h-11zM6 3.5v9",
      panel: "M2.5 3.5h11v9h-11zM10 3.5v9",
      chevron: "M6 3l4 5-4 5",
      open: "M6 3.5H3.5v9h9V10M9.5 2.5H13v3.5M13 2.5L7.5 8",
      eye: "M1.5 8s2.4-4 6.5-4 6.5 4 6.5 4-2.4 4-6.5 4S1.5 8 1.5 8zM8 10a2 2 0 100-4 2 2 0 000 4z",
      history: "M2.5 8a5.5 5.5 0 101.6-3.9M2.5 2v3h3M8 5v3.2l2 1.2",
      link: "M6.5 9.5l3-3M5.5 11a2.5 2.5 0 01-3.5-3.5l2-2a2.5 2.5 0 013.5 0M10.5 5a2.5 2.5 0 013.5 3.5l-2 2a2.5 2.5 0 01-3.5 0",
    };
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("width", size);
    svg.setAttribute("height", size);
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "1.3");
    svg.setAttribute("stroke-linecap", "round");
    svg.setAttribute("stroke-linejoin", "round");
    svg.setAttribute("aria-hidden", "true");

    const path = document.createElementNS("http://www.w3.org/2000/svg", "path");
    path.setAttribute("d", paths[name] || paths.files);
    svg.append(path);
    return svg;
  }

  // ---------- API ----------

  class APIError extends Error {
    constructor(status, code, message) {
      super(message || code || `HTTP ${status}`);
      this.status = status;
      this.code = code;
    }
  }

  async function api(method, path, body) {
    const opts = { method, headers: {}, credentials: "same-origin" };
    if (body instanceof FormData) {
      opts.body = body;
    } else if (body !== undefined) {
      opts.headers["Content-Type"] = "application/json";
      opts.body = JSON.stringify(body);
    }

    const resp = await fetch("/api" + path, opts);

    if (resp.status === 401) throw new APIError(401, "unauthenticated", "not signed in");
    if (resp.status === 204) return null;

    const text = await resp.text();
    let payload = null;
    if (text) {
      try {
        payload = JSON.parse(text);
      } catch {
        payload = { message: text };
      }
    }
    if (!resp.ok) {
      throw new APIError(resp.status, payload && payload.error, payload && payload.message);
    }
    return payload;
  }

  // ---------- feedback ----------

  let toastTimer = null;
  function toast(message, kind = "info") {
    const node = $("#toast");
    clear(node);
    node.append(el("span", { text: message }));
    node.className = "show" + (kind === "error" ? " error" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      node.className = "";
    }, kind === "error" ? 6000 : 2600);
  }

  function fail(err) {
    if (err instanceof APIError && err.status === 401) {
      location.href = "/api/auth/login";
      return;
    }
    toast(err.message || String(err), "error");
  }

  // ---------- formatting ----------

  function humanSize(bytes) {
    if (!bytes) return "0 B";
    const units = ["B", "KB", "MB", "GB", "TB", "PB"];
    const i = Math.min(Math.floor(Math.log(bytes) / Math.log(1024)), units.length - 1);
    const value = bytes / Math.pow(1024, i);
    return `${value >= 10 || i === 0 ? Math.round(value) : value.toFixed(1)} ${units[i]}`;
  }

  function humanTime(iso) {
    if (!iso) return "";
    const then = new Date(iso);
    const seconds = (Date.now() - then.getTime()) / 1000;
    if (seconds < 60) return "just now";
    if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`;
    if (seconds < 86400) return `${Math.floor(seconds / 3600)}h ago`;
    if (seconds < 86400 * 7) return `${Math.floor(seconds / 86400)}d ago`;
    return then.toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }

  function fullTime(iso) {
    if (!iso) return "";
    return new Date(iso).toLocaleString(undefined, {
      year: "numeric",
      month: "short",
      day: "numeric",
      hour: "2-digit",
      minute: "2-digit",
    });
  }

  function clockOf(iso) {
    return new Date(iso).toLocaleTimeString(undefined, {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    });
  }

  function dayOf(iso) {
    return new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric" });
  }

  function joinPath(dir, name) {
    return dir ? `${dir}/${name}` : name;
  }

  function extensionOf(name) {
    const idx = name.lastIndexOf(".");
    if (idx <= 0 || idx === name.length - 1) return "";
    return name.slice(idx + 1);
  }

  function typeClass(entry) {
    if (entry.kind === "dir") return "dir";
    switch (entry.preview) {
      case "image": return "img";
      case "video": return "vid";
      case "audio": return "aud";
      case "pdf": return "doc";
      case "text": return "txt";
    }
    return "";
  }

  function badgeGlyph(entry) {
    if (entry.kind === "dir") return "";
    switch (entry.preview) {
      case "image": return "IMG";
      case "video": return "VID";
      case "audio": return "AUD";
      case "pdf": return "PDF";
      case "text": return "TXT";
    }
    const ext = extensionOf(entry.name);
    return (ext || "FILE").slice(0, 4);
  }

  function kindLabel(entry) {
    if (entry.kind === "dir") return "folder";
    switch (entry.preview) {
      case "image": return "image";
      case "video": return "video";
      case "audio": return "audio";
      case "pdf": return "PDF";
      case "text": return "text";
    }
    const ext = extensionOf(entry.name);
    return ext ? `${ext} file` : "file";
  }

  function isImage(entry) {
    return entry.kind !== "dir" && entry.preview === "image";
  }

  function filePath(entry) {
    return entry.path || joinPath(state.path, entry.name);
  }

  function previewURL(entry) {
    return `/api/files/download?inline=1&path=${encodeURIComponent(filePath(entry))}`;
  }

  function downloadURL(entry) {
    return `/api/files/download?path=${encodeURIComponent(filePath(entry))}`;
  }

  function thumbnailURL(entry, size) {
    // The endpoint takes no size today; the parameter is accepted by the browser's cache
    // key so a future size change does not reuse a stale entry.
    const suffix = size ? `&s=${size}` : "";
    return `/api/files/thumbnail?path=${encodeURIComponent(filePath(entry))}${suffix}`;
  }

  // ---------- selection ----------

  function selectedEntries() {
    return state.entries.filter((e) => state.selected.has(filePath(e)));
  }

  function selectOnly(entry) {
    state.selected.clear();
    if (entry) {
      const p = filePath(entry);
      state.selected.add(p);
      state.anchor = p;
    } else {
      state.anchor = null;
    }
  }

  function toggleSelect(entry) {
    const p = filePath(entry);
    if (state.selected.has(p)) state.selected.delete(p);
    else state.selected.add(p);
    state.anchor = p;
  }

  // extendSelection implements shift-click over the visible order, which is what a person
  // sees, not the underlying array.
  function extendSelection(entry) {
    const list = visibleEntries();
    const to = list.findIndex((e) => filePath(e) === filePath(entry));
    const from = list.findIndex((e) => filePath(e) === state.anchor);
    if (to < 0 || from < 0) {
      selectOnly(entry);
      return;
    }
    const [lo, hi] = from <= to ? [from, to] : [to, from];
    state.selected.clear();
    for (let i = lo; i <= hi; i++) state.selected.add(filePath(list[i]));
  }

  function selectAll() {
    state.selected.clear();
    for (const entry of visibleEntries()) state.selected.add(filePath(entry));
    state.anchor = state.anchor || null;
    renderMain();
    renderInspector();
  }

  function clearSelection() {
    state.selected.clear();
    state.anchor = null;
    renderMain();
    renderInspector();
  }

  function inspectedEntry() {
    if (state.selected.size !== 1) return null;
    const only = [...state.selected][0];
    return state.entries.find((e) => filePath(e) === only) || null;
  }

  // ---------- rail ----------

  const NAV = [
    { key: "files", label: "Files", icon: "files" },
    { key: "shares", label: "Shares", icon: "shares" },
    { key: "trash", label: "Trash", icon: "trash" },
    { key: "audit", label: "Activity", icon: "pulse" },
    { key: "access", label: "Access", icon: "key" },
  ];

  function renderRail() {
    const rail = $("#rail");
    clear(rail);

    rail.append(
      el("div", { class: "rail-head" }, [
        el("span", { class: "mark" }),
        el("span", { class: "word", text: "newcloud" }),
      ]),
    );

    const nav = el("nav", { class: "rail-nav" });
    for (const item of NAV) {
      nav.append(
        el(
          "button",
          {
            "aria-current": state.view === item.key ? "page" : null,
            title: item.label,
            onclick: () => switchTo(item.key),
          },
          [
            el("span", { class: "icon" }, [icon(item.icon)]),
            el("span", { class: "label", text: item.label }),
          ],
        ),
      );
    }
    // The rail toggle belongs with navigation, not in the content header: it is a
    // preference about the chrome, not an action on the files.
    nav.append(
      el(
        "button",
        {
          title: state.railCollapsed ? "Expand sidebar" : "Collapse sidebar",
          onclick: () => {
            state.railCollapsed = !state.railCollapsed;
            document.body.classList.toggle("rail-collapsed", state.railCollapsed);
            renderRail();
          },
        },
        [el("span", { class: "icon" }, [icon("sidebar")]), el("span", { class: "label", text: "Collapse" })],
      ),
    );
    rail.append(nav);

    if (state.me) {
      rail.append(
        el("div", { class: "rail-foot" }, [
          el("span", { class: "avatar", text: (state.me.username || "?").slice(0, 1) }),
          el("span", { class: "who", text: state.me.username }),
          el(
            "button",
            {
              class: "btn ghost icon-only",
              title: "Sign out",
              style: "margin-left:auto",
              onclick: async () => {
                try {
                  await api("POST", "/auth/logout");
                } catch {
                  /* the cookie is gone either way */
                }
                location.href = "/api/auth/login";
              },
            },
            [icon("user", 13)],
          ),
        ]),
      );
    }
  }

  // ---------- content ----------

  function visibleEntries() {
    const q = state.filter.trim().toLowerCase();
    if (!q) return state.entries;
    return state.entries.filter((e) => e.name.toLowerCase().includes(q));
  }

  function renderContent() {
    const content = $("#content");
    clear(content);

    content.append(renderContentHead());

    const progress = el("div", { class: "progress hidden" }, [el("div")]);
    content.append(progress);

    const body = el("div", { class: "content-body" });
    content.append(body);

    switch (state.view) {
      case "files":
        body.append(renderFiles());
        break;
      case "shares":
        body.append(renderShares());
        break;
      case "trash":
        body.append(renderTrash());
        break;
      case "audit":
        body.append(renderAudit());
        break;
      case "access":
        body.append(renderAccess());
        break;
    }
  }

  function renderContentHead() {
    const head = el("div", { class: "content-head" });

    head.append(renderCrumbs());

    if (state.view === "files") {
      // Upload and new-folder live in the head rather than a separate toolbar band: they
      // are the folder's two verbs, and a whole extra row for two buttons cost more than it
      // explained.
      const fileInput = el("input", {
        type: "file",
        multiple: true,
        class: "hidden",
        onchange: (e) => uploadFiles(e.target.files),
      });
      head.append(
        el("button", { class: "btn primary", onclick: () => fileInput.click() }, [
          icon("upload", 13),
          el("span", { text: "Upload" }),
        ]),
        el("button", { class: "btn ghost icon-only", title: "New folder", onclick: () => promptNewFolder() }, [
          icon("folderPlus", 14),
        ]),
        fileInput,
      );

      const search = el("input", {
        type: "search",
        placeholder: "Filter",
        value: state.filter,
        "aria-label": "Filter this folder",
        oninput: (e) => {
          state.filter = e.target.value;
          renderMain();
        },
      });
      head.append(
        el("div", { class: "search" }, [search, el("span", { class: "icon" }, [icon("search", 13)])]),
      );

      head.append(el("span", { class: "spacer" }));

      head.append(
        el("span", { class: "micro num", text: countLabel() }),
        el("div", { class: "seg" }, [
          el(
            "button",
            {
              "aria-pressed": state.layout === "list" ? "true" : "false",
              title: "List",
              onclick: () => setLayout("list"),
            },
            [icon("list", 13)],
          ),
          el(
            "button",
            {
              "aria-pressed": state.layout === "grid" ? "true" : "false",
              title: "Grid",
              onclick: () => setLayout("grid"),
            },
            [icon("grid", 13)],
          ),
        ]),
      );
    } else {
      head.append(el("span", { class: "spacer" }));
    }

    if (state.view !== "files") {
      head.append(el("span", { class: "micro num", text: countLabel() }));
    }

    head.append(
      el(
        "button",
        {
          class: "btn ghost icon-only",
          title: state.inspectorClosed ? "Show details panel" : "Hide details panel",
          style: state.inspectorClosed ? "opacity:0.55" : "",
          onclick: () => {
            state.inspectorClosed = !state.inspectorClosed;
            document.body.classList.toggle("inspector-closed", state.inspectorClosed);
            renderContent();
            renderInspector();
          },
        },
        [icon("panel", 14)],
      ),
    );

    return head;
  }

  function renderCrumbs() {
    const box = el("div", { class: "crumbs" });

    const titles = { shares: "Shared links", trash: "Trash", audit: "Activity", access: "App passwords" };
    if (titles[state.view]) {
      box.append(el("button", { class: "here", text: titles[state.view] }));
      return box;
    }

    box.append(
      el("button", {
        class: state.path ? "" : "here",
        text: "Home",
        onclick: () => navigate(""),
      }),
    );

    if (!state.path) return box;

    let acc = "";
    const segments = state.path.split("/");
    segments.forEach((seg, index) => {
      acc = acc ? `${acc}/${seg}` : seg;
      const target = acc;
      box.append(el("span", { class: "sep", text: "/" }));
      box.append(
        el("button", {
          class: index === segments.length - 1 ? "here" : "",
          text: seg,
          title: seg,
          onclick: () => navigate(target),
        }),
      );
    });
    return box;
  }

  function countLabel() {
    switch (state.view) {
      case "files": {
        const total = state.entries.length;
        const shown = visibleEntries().length;
        if (total === 0) return "";
        if (shown !== total) return `${shown}/${total}`;
        return `${total} item${total === 1 ? "" : "s"}`;
      }
      case "shares":
        return `${state.entries.length} link${state.entries.length === 1 ? "" : "s"}`;
      case "trash":
        return `${state.entries.length} item${state.entries.length === 1 ? "" : "s"}`;
      case "audit":
        return state.audit.total ? `${state.audit.total} events` : "";
      case "access":
        return `${state.entries.length} password${state.entries.length === 1 ? "" : "s"}`;
    }
    return "";
  }

  function setLayout(layout) {
    state.layout = layout;
    renderContent();
  }

  function navigate(path) {
    state.path = path;
    state.filter = "";
    state.selected.clear();
    state.anchor = null;
    state.detail = { path: null, versions: null, shares: null, loading: false };
    loadFiles();
  }

  function emptyState(glyph, title, hint) {
    return el("div", { class: "empty" }, [
      el("div", { class: "glyph" }, [icon(glyph, 26)]),
      el("div", { class: "title", text: title }),
      el("div", { class: "hint", text: hint }),
    ]);
  }

  function renderFiles() {
    const entries = visibleEntries();

    const wrap = el("div", { class: "scroll" });

    if (entries.length === 0) {
      wrap.append(
        emptyState(
          state.filter ? "search" : "files",
          state.filter ? "Nothing matches" : "This folder is empty",
          state.filter ? "Try a different filter." : "Drop files anywhere, or use Upload.",
        ),
      );
      return wrap;
    }

    wrap.append(state.layout === "grid" ? renderGrid(entries) : renderTable(entries));
    return wrap;
  }

  // fileGlyph is the leading square of a row or tile: a thumbnail when the server says the
  // file is an image, otherwise a badge with the extension.
  function fileGlyph(entry) {
    if (isImage(entry)) {
      return el("img", {
        class: "thumb",
        src: thumbnailURL(entry),
        alt: "",
        loading: "lazy",
        // A failed thumbnail must not leave a hole; the badge is the designed fallback.
        onerror: (e) => {
          e.target.replaceWith(el("span", { class: `ftype ${typeClass(entry)}` }, [badgeGlyph(entry)]));
        },
      });
    }
    if (entry.kind === "dir") {
      return el("span", { class: "ftype dir" }, [icon("files", 13)]);
    }
    return el("span", { class: `ftype ${typeClass(entry)}` }, [badgeGlyph(entry)]);
  }

  function renderTable(entries) {
    const table = el("table", { class: "files" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { text: "Name" }),
          el("th", { text: "Kind" }),
          el("th", { class: "num", text: "Size" }),
          el("th", { text: "Modified" }),
        ]),
      ]),
    );

    const body = el("tbody");
    for (const entry of entries) body.append(renderRow(entry));
    table.append(body);

    return el("div", { class: "table-wrap" }, [table]);
  }

  function renderRow(entry) {
    const isDir = entry.kind === "dir";
    const path = filePath(entry);
    const selected = state.selected.has(path);

    const row = el(
      "tr",
      {
        class: selected ? "selected" : "",
        dataset: { path },
        // Single click selects; double click opens. The event detail tells a real double
        // click from a synthetic one.
        onclick: (e) => onRowClick(e, entry),
        ondblclick: () => openEntry(entry),
        oncontextmenu: (e) => {
          e.preventDefault();
          if (!state.selected.has(path)) selectOnly(entry);
          openShareDialog(entry);
        },
      },
      [
        el("td", { class: "name" }, [fileGlyph(entry), el("span", { class: "label" + (isDir ? " dir" : ""), text: entry.name, title: entry.name })]),
        el("td", { class: "kind", text: kindLabel(entry) }),
        el("td", { class: "size num", text: isDir ? "—" : humanSize(entry.size) }),
        el("td", { class: "when", text: humanTime(entry.modified_at) }),
      ],
    );
    return row;
  }

  function onRowClick(e, entry) {
    if (e.shiftKey) {
      extendSelection(entry);
    } else if (e.metaKey || e.ctrlKey) {
      toggleSelect(entry);
    } else {
      selectOnly(entry);
    }
    renderMain();
    renderInspector();
    loadDetailIfNeeded();
  }

  function renderGrid(entries) {
    const grid = el("div", { class: "grid" });
    for (const entry of entries) {
      const isDir = entry.kind === "dir";
      const path = filePath(entry);
      const selected = state.selected.has(path);

      let preview;
      if (isImage(entry)) {
        preview = el("div", { class: "preview" }, [
          el("img", {
            src: thumbnailURL(entry),
            alt: "",
            loading: "lazy",
            onerror: (e) => {
              e.target.replaceWith(el("span", { class: "glyph" }, [icon("doc", 24)]));
            },
          }),
        ]);
      } else {
        const glyphName = isDir
          ? "files"
          : entry.preview === "video"
            ? "download"
            : entry.preview === "text"
              ? "doc"
              : "doc";
        preview = el("div", { class: "preview" + (isDir ? " dir" : "") }, [
          el("span", { class: "glyph" }, [icon(glyphName, 24)]),
        ]);
      }

      grid.append(
        el(
          "div",
          {
            class: "tile" + (selected ? " selected" : ""),
            title: entry.name,
            onclick: (e) => onRowClick(e, entry),
            ondblclick: () => openEntry(entry),
            oncontextmenu: (e) => {
              e.preventDefault();
              if (!state.selected.has(path)) selectOnly(entry);
              openShareDialog(entry);
            },
          },
          [
            preview,
            el("div", { class: "meta" }, [
              el("div", { class: "n", text: entry.name }),
              el("div", { class: "s", text: isDir ? "dir" : humanSize(entry.size) }),
            ]),
          ],
        ),
      );
    }
    return grid;
  }

  // openEntry opens a file: folders navigate, previewable files preview, everything else
  // downloads. Called on double click and on Enter, never on a single click.
  function openEntry(entry) {
    if (entry.kind === "dir") {
      navigate(filePath(entry));
      return;
    }
    if (entry.preview) {
      openPreview(entry);
      return;
    }
    downloadFile(entry);
  }

  async function loadFiles() {
    try {
      const data = await api("GET", `/files/?path=${encodeURIComponent(state.path)}`);
      state.entries = data.children || [];
      renderRail();
      renderContent();
      renderInspector();
    } catch (err) {
      fail(err);
    }
  }

  function downloadFile(entry) {
    const link = el("a", { href: downloadURL(entry), download: entry.name });
    document.body.append(link);
    link.click();
    link.remove();
  }

  // ---------- inspector ----------

  // renderInspector is the other half of the layout: it always shows something useful.
  //
  // With nothing selected it shows the folder itself — name, counts, total size — rather
  // than an empty panel, because a blank column would be worse than no column.
  function renderInspector() {
    const panel = $("#inspector");
    clear(panel);

    if (state.inspectorClosed) return;

    switch (state.view) {
      case "files":
        panel.append(renderFileInspector());
        break;
      case "shares":
        panel.append(renderSimpleInspector("Shares", "Links you have created", "Each link can be limited by password, expiry and use count. Revoke one here; the original URL cannot be shown again, because only its hash is stored."));
        break;
      case "trash":
        panel.append(renderSimpleInspector("Trash", "Restorable files", "Deleted files keep their content. Restoring puts them back at the original path; if that path is taken, the operation is refused rather than overwriting."));
        break;
      case "audit":
        panel.append(renderAuditInspector());
        break;
      case "access":
        panel.append(renderSimpleInspector("Access", "App passwords", "Each password is a separate credential. Revoking one does not end your browser session, which is the point of using them for WebDAV."));
        break;
    }
  }

  function renderSimpleInspector(title, heading, body) {
    return el("div", {}, [
      el("div", { class: "inspector-head" }, [el("span", { class: "title", text: title })]),
      el("div", { class: "inspector-body" }, [
        el("div", { style: "padding:14px" }, [
          el("div", { class: "micro", style: "margin-bottom:6px", text: heading }),
          el("div", { class: "note-empty", text: body }),
        ]),
      ]),
    ]);
  }

  function renderFileInspector() {
    const frag = document.createDocumentFragment();

    const entry = inspectedEntry();

    frag.append(
      el("div", { class: "inspector-head" }, [
        el("span", { class: "title", text: entry ? "Details" : "This folder" }),
        entry
          ? el("button", { class: "btn ghost icon-only", title: "Close selection", onclick: () => clearSelection() }, [icon("close", 13)])
          : null,
      ]),
    );

    const body = el("div", { class: "inspector-body" });

    if (!entry) {
      body.append(renderFolderSummary());
      frag.append(body);
      return frag;
    }

    body.append(renderStage(entry));
    body.append(
      el("div", { class: "inspector-file" }, [
        el("div", { class: "name", text: entry.name }),
        el("div", { class: "path", text: "/" + filePath(entry) }),
      ]),
    );

    body.append(renderProps(entry));
    body.append(renderActions(entry));

    if (entry.kind !== "dir") {
      body.append(renderVersionsSection(entry));
      body.append(renderSharesSection(entry));
    }

    frag.append(body);
    return frag;
  }

  // renderStage is the preview well. It shows the image when the server allows one, a glyph
  // otherwise, and an explicit reason when a preview exists but cannot be shown here.
  function renderStage(entry) {
    const stage = el("div", { class: "stage" });

    if (isImage(entry)) {
      stage.append(
        el("img", {
          src: previewURL(entry),
          alt: "",
          onerror: () => {
            clear(stage);
            stage.append(el("span", { class: "glyph" }, [icon("doc", 26)]));
            stage.append(el("div", { class: "note", text: "Preview unavailable" }));
          },
        }),
      );
    } else if (entry.kind === "dir") {
      stage.append(el("span", { class: "glyph" }, [icon("files", 26)]));
    } else if (entry.preview === "text") {
      // Text has a preview but no thumbnail, so an icon would waste the well. Show the
      // first lines instead: it answers "which file is this" without opening it, and the
      // bytes still go through textContent.
      stage.classList.add("text-stage");
      const pre = el("pre", { text: "" });
      stage.append(pre);
      fetchTextHead(entry, pre);
    } else if (entry.preview) {
      stage.append(el("span", { class: "glyph" }, [icon("doc", 26)]));
    } else {
      stage.append(el("div", { class: "note", text: "No inline preview for this type" }));
    }

    if (entry.kind !== "dir" && entry.preview) {
      stage.append(
        el("div", {
          class: "open-hint",
          text: "Open preview",
          onclick: () => openPreview(entry),
        }),
      );
    }

    return stage;
  }

  // fetchTextHead loads the beginning of a text file into the preview well.
  //
  // Only the first few kilobytes: the point is recognition, not reading, and a 5 MB log
  // should not be pulled across the network for a 178-pixel panel. The response is the
  // same inline endpoint, so the server's type and content checks apply unchanged.
  async function fetchTextHead(entry, pre) {
    try {
      const resp = await fetch(previewURL(entry), { credentials: "same-origin" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const text = await resp.text();
      // A fixed number of lines rather than a byte count: the well is measured in lines, so
      // counting lines is what keeps it from looking cut off mid-sentence.
      const lines = text.split("\n").slice(0, 9);
      pre.textContent = lines.join("\n");
    } catch {
      pre.textContent = "Could not load a preview.";
    }
  }

  function renderProps(entry) {    const dl = el("dl");
    const add = (key, value, mono) => {
      dl.append(el("dt", { text: key }));
      dl.append(el("dd", { class: mono ? "mono" : "", text: value }));
    };

    add("Kind", kindLabel(entry));
    if (entry.kind !== "dir") add("Size", humanSize(entry.size));
    add("Modified", fullTime(entry.modified_at));
    if (entry.mime) add("Type", entry.mime, true);
    if (entry.kind !== "dir" && entry.preview) {
      add("Preview", entry.preview === "pdf" ? "PDF viewer" : entry.preview);
    }

    return el("div", { class: "props" }, [dl]);
  }

  function renderActions(entry) {
    const row = el("div", { class: "inspector-actions" });

    if (entry.kind !== "dir" && entry.preview) {
      row.append(
        el("button", { class: "btn small", onclick: () => openPreview(entry) }, [icon("eye", 13), el("span", { text: "Open" })]),
      );
    }
    if (entry.kind !== "dir") {
      row.append(
        el("button", { class: "btn small", onclick: () => downloadFile(entry) }, [icon("download", 13), el("span", { text: "Download" })]),
      );
    }
    row.append(
      el("button", { class: "btn small", onclick: () => openShareDialog(entry) }, [icon("link", 13), el("span", { text: "Share" })]),
    );
    row.append(
      el("button", { class: "btn small", onclick: () => promptRename(entry) }, [el("span", { text: "Rename" })]),
    );
    row.append(
      el("button", { class: "btn small danger", onclick: () => confirmDelete(entry) }, [el("span", { text: "Delete" })]),
    );

    return row;
  }

  // renderFolderSummary fills the inspector when nothing is selected.
  function renderFolderSummary() {
    const wrap = el("div", {});

    const files = state.entries.filter((e) => e.kind !== "dir");
    const dirs = state.entries.filter((e) => e.kind === "dir");
    const total = files.reduce((sum, e) => sum + (e.size || 0), 0);

    wrap.append(
      el("div", { class: "inspector-file" }, [
        el("div", { class: "name", text: state.path ? state.path.split("/").pop() : "Home" }),
        el("div", { class: "path", text: "/" + state.path }),
      ]),
    );

    const dl = el("dl");
    dl.append(el("dt", { text: "Files" }), el("dd", { text: String(files.length) }));
    dl.append(el("dt", { text: "Folders" }), el("dd", { text: String(dirs.length) }));
    dl.append(el("dt", { text: "Total size" }), el("dd", { text: humanSize(total) }));
    wrap.append(el("div", { class: "props" }, [dl]));

    wrap.append(
      el("div", { class: "inspector-empty" }, [
        el("div", { class: "glyph" }, [icon("eye", 22)]),
        el("div", { class: "title", text: "Nothing selected" }),
        el("div", {
          class: "hint",
          text: "Select a file to see its details, version history and shares. Double-click or press Enter to open.",
        }),
      ]),
    );

    return wrap;
  }

  // renderVersionsSection lists the stored versions of a file.
  //
  // The data has existed since the DAV layer was written and the interface never showed it;
  // a version you cannot see is a version you cannot use.
  function renderVersionsSection(entry) {
    const section = el("details", { class: "inspector-section" });
    const bodyWrap = el("div", { class: "section-body" });

    const loading = state.detail.path !== filePath(entry) || state.detail.versions == null;

    section.append(
      el("summary", {}, [
        el("span", { class: "chev" }, [icon("chevron", 11)]),
        el("span", { class: "micro", text: "Version history" }),
        el("span", { class: "count", text: loading ? "…" : String(state.detail.versions.length) }),
      ]),
    );

    if (loading) {
      bodyWrap.append(el("div", { class: "note-empty", text: "Loading…" }));
    } else if (state.detail.versions.length === 0) {
      bodyWrap.append(
        el("div", { class: "note-empty", text: "No earlier versions. A new version is stored each time the file is overwritten." }),
      );
    } else {
      for (const v of state.detail.versions) {
        bodyWrap.append(
          el("div", { class: "version-row" }, [
            el("span", { class: "vno", text: `v${v.version}` }),
            el("span", { class: "vmeta", text: `${humanSize(v.size)} · ${humanTime(v.created_at)}` }),
            el("button", {
              class: "row-action act",
              text: "Restore",
              onclick: () => restoreVersion(entry, v),
            }),
          ]),
        );
      }
    }

    section.append(bodyWrap);
    return section;
  }

  // renderSharesSection lists the links pointing at this file.
  function renderSharesSection(entry) {
    const section = el("details", { class: "inspector-section" });
    const bodyWrap = el("div", { class: "section-body" });

    const path = filePath(entry);
    const loading = state.detail.path !== path || state.detail.shares == null;
    const shares = state.detail.shares || [];

    section.append(
      el("summary", {}, [
        el("span", { class: "chev" }, [icon("chevron", 11)]),
        el("span", { class: "micro", text: "Shared by" }),
        el("span", { class: "count", text: loading ? "…" : String(shares.length) }),
      ]),
    );

    if (loading) {
      bodyWrap.append(el("div", { class: "note-empty", text: "Loading…" }));
    } else if (shares.length === 0) {
      bodyWrap.append(el("div", { class: "note-empty", text: "Not shared. Use Share to create a link." }));
    } else {
      for (const s of shares) {
        const used =
          s.kind === "request"
            ? `${s.upload_count}${s.max_uploads ? "/" + s.max_uploads : ""} in`
            : `${s.download_count}${s.max_downloads ? "/" + s.max_downloads : ""} out`;
        bodyWrap.append(
          el("div", { class: "share-row" }, [
            el("span", { class: "pill" + (s.kind === "request" ? " accent" : ""), text: s.kind }),
            el("span", { class: "used", text: used }),
            // No copy action. Only the token's hash is stored, so the URL cannot be
            // reconstructed — offering a button that could never work is worse than saying
            // why it is absent.
            el("span", { class: "note-empty", text: "link not recoverable" }),
            el("button", {
              class: "row-action danger",
              text: "Revoke",
              onclick: async () => {
                try {
                  await api("DELETE", `/shares/${s.id}`);
                  toast("Share revoked");
                  reloadDetail(entry);
                  if (state.view === "shares") loadShares();
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        );
      }
    }

    section.append(bodyWrap);
    return section;
  }

  // loadDetailIfNeeded fetches versions and shares for the single selected file.
  //
  // Both are small and both are needed the moment a row is selected, so they load together
  // rather than waiting for a section to be expanded — an expanded-but-empty section reads
  // as a bug.
  async function loadDetailIfNeeded() {
    const entry = inspectedEntry();
    if (!entry || entry.kind === "dir") return;

    const path = filePath(entry);
    if (state.detail.path === path && state.detail.versions != null) return;

    state.detail = { path, versions: null, shares: null, loading: true };

    const [versions, shares] = await Promise.all([
      api("GET", `/files/versions?path=${encodeURIComponent(path)}`).catch(() => ({ versions: [] })),
      api("GET", "/shares/").catch(() => ({ items: [] })),
    ]);

    // The selection may have moved while these were in flight; discard stale results rather
    // than showing another file's history.
    if (state.detail.path !== path) return;

    state.detail = {
      path,
      versions: versions.versions || [],
      // Only the links pointing at this node.
      shares: (shares.items || []).filter((s) => s.node_id === entry.id),
      loading: false,
    };
    renderInspector();
  }

  function reloadDetail(entry) {
    state.detail = { path: null, versions: null, shares: null, loading: false };
    renderInspector();
    loadDetailIfNeeded();
  }

  async function restoreVersion(entry, version) {
    try {
      await api("POST", "/files/restore-version", {
        path: filePath(entry),
        version: version.version,
      });
      toast(`Restored v${version.version}`);
      await loadFiles();
      reloadDetail(entry);
    } catch (err) {
      fail(err);
    }
  }

  // ---------- preview ----------

  let activeDialog = null;

  function openPreview(entry) {
    const kind = entry.preview;
    const url = previewURL(entry);
    const body = el("div", { class: "preview-body" });

    switch (kind) {
      case "image":
        body.append(
          el("img", {
            src: url,
            alt: entry.name,
            onerror: () => {
              clear(body);
              body.append(unsupportedNote("This image could not be displayed."));
            },
          }),
        );
        break;

      case "video":
        body.append(
          el("video", {
            src: url,
            controls: true,
            controlslist: "nodownload",
            preload: "metadata",
            onerror: () => {
              clear(body);
              body.append(unsupportedNote("This video format is not supported by the browser."));
            },
          }),
        );
        break;

      case "audio":
        body.append(
          el("audio", {
            src: url,
            controls: true,
            preload: "metadata",
            onerror: () => {
              clear(body);
              body.append(unsupportedNote("This audio format is not supported by the browser."));
            },
          }),
        );
        break;

      case "pdf":
        // No sandbox attribute: Chrome refuses to instantiate its PDF plugin inside a
        // sandboxed frame and shows a broken-page icon instead. The response carries
        // `sandbox` in its CSP, which applies to whatever the frame loads.
        body.append(el("iframe", { src: url, title: entry.name }));
        break;

      case "text":
        body.append(el("pre", { text: "Loading…" }));
        loadTextPreview(url, body);
        break;

      default:
        body.append(unsupportedNote("No preview for this file type."));
    }

    const dialog = el("dialog", { class: "preview" }, [
      el("div", { class: "preview-head" }, [
        fileGlyph(entry),
        el("span", { class: "title", text: entry.name, title: entry.name }),
        el("span", { class: "sub num", text: humanSize(entry.size) }),
        el("span", { class: "spacer" }),
        el("button", { class: "btn ghost", onclick: () => downloadFile(entry) }, [
          icon("download", 14),
          el("span", { text: "Download" }),
        ]),
        el(
          "button",
          { class: "btn ghost icon-only", "aria-label": "Close", title: "Close", onclick: () => dialog.close() },
          [icon("close", 14)],
        ),
      ]),
      body,
    ]);

    showDialog(dialog);
  }

  function unsupportedNote(message) {
    return el("div", { class: "unsupported" }, [
      el("strong", { text: "Preview unavailable" }),
      el("span", { text: message }),
    ]);
  }

  // loadTextPreview fetches the bytes and inserts them as text.
  //
  // textContent, not innerHTML: a .html file previewed here is shown as source, and parsing
  // it would execute it. The server also refuses to inline HTML, so this is the second of
  // two independent defences.
  async function loadTextPreview(url, container) {
    try {
      const resp = await fetch(url, { credentials: "same-origin" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const text = await resp.text();
      clear(container);
      container.append(el("pre", { text }));
    } catch (err) {
      clear(container);
      container.append(unsupportedNote(`Could not load the file: ${err.message}`));
    }
  }

  // ---------- uploads ----------

  let uploading = false;

  async function uploadFiles(fileList, targetPath) {
    if (!fileList || fileList.length === 0) return;
    if (uploading) {
      toast("An upload is already running", "error");
      return;
    }

    const dest = targetPath == null ? state.path : targetPath;
    uploading = true;

    const progress = $("#content .progress");
    const bar = progress && progress.firstElementChild;
    const total = fileList.length;
    let done = 0;
    let failed = 0;

    if (progress) progress.classList.remove("hidden");

    for (const file of fileList) {
      const form = new FormData();
      form.append("path", dest);
      form.append("file", file);

      try {
        await api("POST", "/files/upload", form);
        done++;
      } catch (err) {
        failed++;
        toast(`${file.name}: ${err.message || "failed"}`, "error");
      }
      if (bar) bar.style.width = `${Math.round(((done + failed) / total) * 100)}%`;
    }

    uploading = false;
    if (progress) {
      setTimeout(() => {
        progress.classList.add("hidden");
        if (bar) bar.style.width = "0";
      }, 350);
    }

    if (done > 0) {
      toast(
        failed > 0
          ? `Uploaded ${done}, ${failed} failed`
          : done === 1
            ? "Uploaded 1 file"
            : `Uploaded ${done} files`,
        failed > 0 ? "error" : "info",
      );
    }

    if (dest === state.path) await loadFiles();
  }

  // ---------- dialogs ----------

  function showDialog(dialog) {
    if (activeDialog) activeDialog.close();
    document.body.append(dialog);
    activeDialog = dialog;
    dialog.addEventListener("close", () => {
      dialog.remove();
      if (activeDialog === dialog) activeDialog = null;
    });
    dialog.showModal();
  }

  function formDialog({ title, body, fields = [], confirm, danger, onSubmit, extra, confirmLabel }) {
    const form = el("form", { method: "dialog" });
    const values = {};

    form.append(
      el("div", { class: "dlg-head" }, [
        el("h2", { text: title }),
        body ? el("p", { text: body }) : null,
      ]),
    );

    const bodyWrap = el("div", { class: "dlg-body" });
    if (extra) bodyWrap.append(extra);

    for (const field of fields) {
      let input;
      if (field.type === "select") {
        input = el("select", { name: field.name });
        for (const opt of field.options) {
          input.append(el("option", { value: opt.value, text: opt.label }));
        }
        if (field.value) input.value = field.value;
      } else {
        input = el("input", {
          type: field.type,
          name: field.name,
          value: field.value == null ? "" : field.value,
          placeholder: field.placeholder || "",
          min: field.min,
          max: field.max,
        });
      }
      values[field.name] = input;
      bodyWrap.append(
        el("label", { class: "field" }, [el("span", { class: "micro", text: field.label }), input]),
      );
    }
    form.append(bodyWrap);

    const submit = el("button", {
      class: danger ? "btn danger" : "btn primary",
      text: confirmLabel || confirm,
      type: "submit",
    });
    const cancel = el("button", {
      class: "btn",
      text: "Cancel",
      type: "button",
      onclick: () => dialog.close(),
    });
    form.append(el("menu", {}, [cancel, submit]));

    const dialog = el("dialog", {}, [form]);

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const out = {};
      for (const [name, input] of Object.entries(values)) out[name] = input.value.trim();

      submit.disabled = true;
      try {
        await onSubmit(out);
        dialog.close();
      } catch (err) {
        fail(err);
        submit.disabled = false;
      }
    });

    showDialog(dialog);
    const first = fields.length > 0 ? values[fields[0].name] : null;
    if (first) {
      first.focus();
      if (first.select) first.select();
    }
    return dialog;
  }

  function promptNewFolder() {
    formDialog({
      title: "New folder",
      fields: [{ name: "name", label: "Name", type: "text" }],
      confirmLabel: "Create",
      onSubmit: async (v) => {
        if (!v.name) throw new Error("a folder needs a name");
        await api("POST", "/files/mkdir", { path: joinPath(state.path, v.name) });
        toast("Folder created");
        await loadFiles();
      },
    });
  }

  function promptRename(entry) {
    formDialog({
      title: "Rename",
      body: entry.name,
      fields: [{ name: "name", label: "New name", type: "text", value: entry.name }],
      confirmLabel: "Rename",
      onSubmit: async (v) => {
        if (!v.name || v.name === entry.name) return;
        await api("POST", "/files/rename", { path: filePath(entry), name: v.name });
        toast("Renamed");
        await loadFiles();
      },
    });
  }

  function confirmDelete(entry) {
    const entries = state.selected.size > 1 ? selectedEntries() : [entry];
    const what = entries.length === 1 ? entries[0].name : `${entries.length} items`;

    formDialog({
      title: `Delete ${what}?`,
      body: entries.length === 1
        ? "It moves to the trash and can be restored from there."
        : "They move to the trash and can be restored from there.",
      fields: [],
      confirm: "Delete",
      danger: true,
      onSubmit: async () => {
        for (const target of entries) {
          await api("DELETE", `/files/?path=${encodeURIComponent(filePath(target))}`);
        }
        toast(entries.length === 1 ? "Moved to trash" : `Moved ${entries.length} items to trash`);
        clearSelection();
        await loadFiles();
      },
    });
  }

  function openShareDialog(entry) {
    const path = filePath(entry);
    const isDir = entry.kind === "dir";

    formDialog({
      title: `Share ${entry.name}`,
      fields: [
        {
          name: "kind",
          label: "Type",
          type: "select",
          options: [
            { value: "link", label: "Link — anyone with the URL can download" },
            ...(isDir
              ? [{ value: "request", label: "Request — others can upload into this folder" }]
              : []),
          ],
        },
        { name: "password", label: "Password (optional)", type: "password" },
        { name: "expires_hours", label: "Expires in hours (optional)", type: "number", min: "0" },
        { name: "max_uses", label: "Maximum uses (optional)", type: "number", min: "0" },
      ],
      confirmLabel: "Create link",
      onSubmit: async (v) => {
        const payload = { path, kind: v.kind };
        if (v.password) payload.password = v.password;

        const hours = parseInt(v.expires_hours, 10);
        if (hours > 0) payload.expires_in_seconds = hours * 3600;

        const max = parseInt(v.max_uses, 10);
        if (max > 0) {
          if (v.kind === "request") payload.max_uploads = max;
          else payload.max_downloads = max;
        }

        const created = await api("POST", "/shares/", payload);
        const token = created.token;
        const url = token ? `${location.origin}/s/${token}` : "";

        if (url) {
          await navigator.clipboard.writeText(url).catch(() => {});
          formDialog({
            title: "Link created",
            body: "Copied to the clipboard. The token is not shown again.",
            fields: [],
            confirm: "Done",
            extra: el("div", { class: "copybox", text: url }),
            onSubmit: async () => {},
          });
        } else {
          toast("Share created");
        }
        reloadDetail(entry);
      },
    });
  }

  // ---------- shares view ----------

  function renderShares() {
    const wrap = el("div", { class: "scroll" });

    if (state.entries.length === 0) {
      wrap.append(emptyState("shares", "No shares yet", "Open a file and choose Share to create a link."));
      return wrap;
    }

    const table = el("table", { class: "files" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { text: "Target" }),
          el("th", { text: "Kind" }),
          el("th", { class: "num", text: "Uses" }),
          el("th", { text: "Expires" }),
          el("th", { text: "" }),
        ]),
      ]),
    );

    const body = el("tbody");
    for (const share of state.entries) {
      const uses =
        share.kind === "request"
          ? `${share.upload_count}${share.max_uploads ? " / " + share.max_uploads : ""}`
          : `${share.download_count}${share.max_downloads ? " / " + share.max_downloads : ""}`;

      body.append(
        el("tr", {}, [
          el("td", { class: "name" }, [
            el("span", {
              class: "label",
              text: share.node_path || share.label || "—",
              title: "Copy this link",
              onclick: () => {
                const url = `${location.origin}/s/${share.token}`;
                navigator.clipboard.writeText(url).then(
                  () => toast("Link copied"),
                  () => toast(url),
                );
              },
            }),
          ]),
          el("td", { class: "kind" }, [
            el("span", { class: "pill" + (share.kind === "request" ? " accent" : ""), text: share.kind }),
          ]),
          el("td", { class: "size num", text: uses }),
          el("td", { class: "when", text: share.expires_at ? new Date(share.expires_at).toLocaleDateString() : "never" }),
          el("td", { class: "actions", style: "text-align:right" }, [
            el("button", {
              class: "row-action danger",
              text: "Revoke",
              onclick: async () => {
                try {
                  await api("DELETE", `/shares/${share.id}`);
                  toast("Share revoked");
                  loadShares();
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        ]),
      );
    }
    table.append(body);
    wrap.append(el("div", { class: "table-wrap" }, [table]));
    return wrap;
  }

  async function loadShares() {
    try {
      const data = await api("GET", "/shares/");
      state.entries = data.items || [];
      renderContent();
      renderInspector();
    } catch (err) {
      fail(err);
    }
  }

  // ---------- trash view ----------

  function renderTrash() {
    const wrap = el("div", { class: "scroll" });

    if (state.entries.length === 0) {
      wrap.append(emptyState("trash", "Trash is empty", "Deleted files appear here and can be restored."));
      return wrap;
    }

    const table = el("table", { class: "files" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { text: "Name" }),
          el("th", { text: "Kind" }),
          el("th", { class: "num", text: "Size" }),
          el("th", { text: "" }),
        ]),
      ]),
    );
    const body = el("tbody");
    for (const entry of state.entries) {
      body.append(
        el("tr", {}, [
          el("td", { class: "name" }, [fileGlyph(entry), el("span", { class: "label", text: entry.name })]),
          el("td", { class: "kind", text: kindLabel(entry) }),
          el("td", { class: "size num", text: entry.kind === "dir" ? "—" : humanSize(entry.size) }),
          el("td", { class: "actions", style: "text-align:right" }, [
            el("button", {
              class: "row-action",
              text: "Restore",
              onclick: async () => {
                try {
                  await api("POST", "/files/restore", { id: entry.id });
                  toast("Restored");
                  loadTrash();
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        ]),
      );
    }
    table.append(body);
    wrap.append(el("div", { class: "table-wrap" }, [table]));
    return wrap;
  }

  async function loadTrash() {
    try {
      const data = await api("GET", "/files/trash");
      state.entries = data.items || [];
      renderContent();
      renderInspector();
    } catch (err) {
      fail(err);
    }
  }

  // ---------- audit view ----------

  function personLabel(ev) {
    if (ev.actor_email) return ev.actor_email;
    if (ev.share_id) return "via link";
    if (ev.actor_kind === "anonymous" || !ev.actor_kind) return "anonymous";
    if (ev.actor_kind === "system") return "system";
    return ev.actor_kind;
  }

  function describeTarget(ev) {
    const name = ev.metadata && (ev.metadata.name || ev.metadata.path);
    if (name) return String(name);
    if (ev.share_id) return `share ${ev.share_id.slice(0, 8)}`;
    if (ev.target_id) return ev.target_id.slice(0, 8);
    return "";
  }

  function renderAudit() {
    const wrap = el("div", { style: "display:flex;flex-direction:column;min-height:0;flex:1" });
    const a = state.audit;

    const actionSelect = el("select", {
      "aria-label": "Filter by action",
      onchange: (e) => {
        a.action = e.target.value;
        reloadAudit();
      },
    });
    actionSelect.append(el("option", { value: "", text: "All actions" }));
    for (const action of a.actions) {
      const opt = el("option", { value: action, text: action });
      if (action === a.action) opt.selected = true;
      actionSelect.append(opt);
    }

    const resultSelect = el("select", {
      "aria-label": "Filter by result",
      onchange: (e) => {
        a.result = e.target.value;
        reloadAudit();
      },
    });
    for (const [value, label] of [
      ["", "Any result"],
      ["ok", "Allowed"],
      ["denied", "Refused"],
    ]) {
      const opt = el("option", { value, text: label });
      if (value === a.result) opt.selected = true;
      resultSelect.append(opt);
    }

    const sinceSelect = el("select", {
      "aria-label": "Filter by time",
      onchange: (e) => {
        a.since = e.target.value;
        reloadAudit();
      },
    });
    for (const [value, label] of [
      ["", "All time"],
      ["1h", "Last hour"],
      ["24h", "Last 24 hours"],
      ["7d", "Last 7 days"],
      ["30d", "Last 30 days"],
    ]) {
      const opt = el("option", { value, text: label });
      if (value === a.since) opt.selected = true;
      sinceSelect.append(opt);
    }

    wrap.append(
      el("div", { class: "audit-toolbar" }, [
        actionSelect,
        resultSelect,
        sinceSelect,
        el("span", { class: "spacer", style: "flex:1" }),
        el(
          "button",
          {
            class: "btn ghost small",
            title: "Download the full log as CSV",
            onclick: () => {
              const q = new URLSearchParams({ format: "csv" });
              if (a.action) q.set("actions", a.action);
              if (a.result) q.set("result", a.result);
              const since = sinceISO(a.since);
              if (since) q.set("since", since);
              location.href = `/api/audit?${q.toString()}`;
            },
          },
          [icon("download", 13), el("span", { text: "Export" })],
        ),
        el("button", { class: "btn ghost icon-only", title: "Refresh", onclick: () => reloadAudit() }, [icon("refresh", 14)]),
      ]),
    );

    const scroll = el("div", { class: "scroll" });

    if (a.events.length === 0) {
      scroll.append(
        emptyState(
          "pulse",
          a.loading ? "Loading…" : "No activity",
          a.action || a.result || a.since ? "Nothing matches these filters." : "Events appear here as files are touched.",
        ),
      );
      wrap.append(scroll);
      return wrap;
    }

    const timeline = el("div", { class: "timeline" });
    let lastDay = "";

    for (const ev of a.events) {
      const day = dayOf(ev.ts);
      if (day !== lastDay) {
        lastDay = day;
        timeline.append(el("div", { class: "audit-day", text: day }));
      }

      const target = describeTarget(ev);
      const meta = el("div", { class: "meta" });
      if (ev.actor_ip) meta.append(el("span", {}, [el("span", { class: "v", text: ev.actor_ip })]));
      if (ev.bytes) meta.append(el("span", {}, [el("span", { class: "v", text: humanSize(ev.bytes) })]));
      if (ev.share_id) meta.append(el("span", {}, [el("span", { class: "v", text: `share ${ev.share_id.slice(0, 8)}` })]));

      timeline.append(
        el("div", { class: `event ${ev.result === "denied" ? "denied" : "ok"}` }, [
          el("div", { class: "when" }, [
            el("span", { class: "day", text: clockOf(ev.ts) }),
            ev.result === "denied" ? "refused" : "allowed",
          ]),
          el("div", { class: "rail" }, [el("span", { class: "dot" })]),
          el("div", { class: "body" }, [
            el("div", { class: "headline" }, [
              el("span", { class: "action", text: ev.action }),
              target ? el("span", { class: "target", text: target, title: target }) : null,
              el("span", { class: "who", text: personLabel(ev) }),
            ]),
            meta.childNodes.length > 0 ? meta : null,
          ]),
        ]),
      );
    }

    scroll.append(timeline);

    if (a.hasMore) {
      scroll.append(
        el("div", { class: "audit-foot" }, [
          el("button", {
            class: "btn",
            text: "Load older events",
            onclick: async () => {
              state.audit.offset += state.audit.events.length;
              await loadAudit(true);
            },
          }),
        ]),
      );
    } else {
      scroll.append(el("div", { class: "audit-foot" }, [el("span", { text: "End of the log" })]));
    }

    wrap.append(scroll);
    return wrap;
  }

  function renderAuditInspector() {
    const a = state.audit;
    const denied = a.events.filter((e) => e.result === "denied").length;

    const dl = el("dl");
    dl.append(el("dt", { text: "Events" }), el("dd", { text: String(a.total) }));
    dl.append(el("dt", { text: "Refused" }), el("dd", { text: String(denied) }));
    dl.append(el("dt", { text: "Showing" }), el("dd", { text: String(a.events.length) }));

    return el("div", {}, [
      el("div", { class: "inspector-head" }, [el("span", { class: "title", text: "Reading the log" })]),
      el("div", { class: "inspector-body" }, [
        el("div", { class: "props" }, [dl]),
        el("div", { style: "padding:12px 14px" }, [
          el("div", { class: "micro", style: "margin-bottom:6px", text: "Scope" }),
          el("div", {
            class: "note-empty",
            text:
              "You see what you did, plus everything that happened to your own files and links — including downloads by anonymous visitors and refused attempts. Administrators see the whole instance.",
          }),
          el("div", { class: "micro", style: "margin:12px 0 6px", text: "Integrity" }),
          el("div", {
            class: "note-empty",
            text: "Rows are hash-chained in the order written. An edited or removed row breaks the chain, which is what the export verifies.",
          }),
        ]),
      ]),
    ]);
  }

  function sinceISO(choice) {
    if (!choice) return "";
    const seconds = { "1h": 3600, "24h": 86400, "7d": 604800, "30d": 2592000 }[choice];
    if (!seconds) return "";
    return new Date(Date.now() - seconds * 1000).toISOString();
  }

  function reloadAudit() {
    state.audit.events = [];
    state.audit.offset = 0;
    loadAudit(false);
  }

  async function loadAudit(append) {
    const a = state.audit;
    if (a.loading) return;
    a.loading = true;

    const q = new URLSearchParams();
    q.set("offset", String(append ? a.offset : 0));
    if (a.action) q.set("actions", a.action);
    if (a.result) q.set("result", a.result);
    const since = sinceISO(a.since);
    if (since) q.set("since", since);

    try {
      const data = await api("GET", `/audit/events?${q.toString()}`);
      a.events = append ? a.events.concat(data.events || []) : data.events || [];
      a.total = data.total || 0;
      a.hasMore = !!data.has_more;
      a.actions = data.actions || [];
      if (!append) a.offset = 0;
    } catch (err) {
      fail(err);
    } finally {
      a.loading = false;
      renderContent();
      renderInspector();
    }
  }

  // ---------- access view ----------

  function renderAccess() {
    const wrap = el("div", { class: "scroll" });

    wrap.append(
      el("div", { style: "padding:14px" }, [
        el("div", { class: "card" }, [
          el("h2", { text: "Connecting a client" }),
          el("p", {
            class: "hint",
            text:
              "Use an app password instead of your browser session for WebDAV, rclone or a mounted drive. " +
              "Each one can be revoked on its own, so a leaked credential does not end your browser login.",
          }),
          el("div", {
            class: "copybox",
            text: `${location.origin}/dav/${state.me ? state.me.username : "you"}/`,
          }),
          el("div", { style: "margin-top:12px" }, [
            el("button", { class: "btn primary", onclick: () => createAppPassword() }, [
              icon("key", 14),
              el("span", { text: "New app password" }),
            ]),
          ]),
        ]),
      ]),
    );

    if (state.entries.length === 0) return wrap;

    const table = el("table", { class: "files" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { text: "Label" }),
          el("th", { text: "Scope" }),
          el("th", { text: "Last used" }),
          el("th", { text: "" }),
        ]),
      ]),
    );
    const body = el("tbody");
    for (const cred of state.entries) {
      body.append(
        el("tr", {}, [
          el("td", { class: "name" }, [el("span", { class: "label", text: cred.label || "—" })]),
          el("td", { class: "kind" }, [el("span", { class: "pill", text: cred.scope })]),
          el("td", { class: "when", text: cred.last_used ? humanTime(cred.last_used) : "never" }),
          el("td", { class: "actions", style: "text-align:right" }, [
            el("button", {
              class: "row-action danger",
              text: "Revoke",
              onclick: async () => {
                try {
                  await api("DELETE", `/apppasswords/${cred.id}`);
                  toast("Revoked");
                  loadCredentials();
                } catch (err) {
                  fail(err);
                }
              },
            }),
          ]),
        ]),
      );
    }
    table.append(body);
    wrap.append(el("div", { class: "table-wrap" }, [table]));
    return wrap;
  }

  async function loadCredentials() {
    try {
      const data = await api("GET", "/apppasswords/");
      state.entries = data.items || [];
      renderContent();
      renderInspector();
    } catch (err) {
      fail(err);
    }
  }

  function createAppPassword() {
    formDialog({
      title: "New app password",
      fields: [
        { name: "label", label: "Label", type: "text", placeholder: "laptop, phone, rclone" },
        {
          name: "scope",
          label: "Scope",
          type: "select",
          options: [
            { value: "rw", label: "Read and write" },
            { value: "ro", label: "Read only" },
          ],
        },
      ],
      confirmLabel: "Create",
      onSubmit: async (v) => {
        const created = await api("POST", "/apppasswords/", v);
        formDialog({
          title: "Copy this password now",
          body: "It cannot be shown again. Use your username with this value as the password.",
          fields: [],
          confirm: "Done",
          extra: el("div", { class: "copybox", text: created.secret || "" }),
          onSubmit: async () => {
            await loadCredentials();
          },
        });
      },
    });
  }

  // ---------- share landing ----------

  async function renderShareLanding(token) {
    const root = $("#app");
    clear(root);
    // .share-page positions itself fixed, which takes it out of the #app grid (that grid
    // exists for the signed-in shell and would otherwise give this page the rail's width).

    let info;
    try {
      const resp = await fetch(`/s/${encodeURIComponent(token)}`, { headers: { Accept: "application/json" } });
      if (!resp.ok) throw new Error("not available");
      info = await resp.json();
    } catch {
      root.append(
        el("div", { class: "share-page" }, [
          el("div", { class: "share-card" }, [
            el("div", { class: "head" }, [
              el("div", { class: "mark" }),
              el("div", { class: "title", text: "Link not available" }),
              el("div", { class: "meta", text: "It may have expired, been revoked, or reached its usage limit." }),
            ]),
          ]),
        ]),
      );
      return;
    }

    const card = el("div", { class: "share-card" });
    const head = el("div", { class: "head" }, [
      el("div", { class: "mark" }),
      el("div", { class: "title", text: info.label || "Shared files" }),
    ]);
    card.append(head);

    const body = el("div", { class: "body" });

    if (info.requires_password) {
      head.append(el("div", { class: "meta", text: "This link is protected by a password." }));
      const input = el("input", { type: "password", placeholder: "Password" });
      const submit = async () => {
        try {
          const resp = await fetch(`/s/${encodeURIComponent(token)}/verify`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ password: input.value }),
          });
          if (!resp.ok) {
            toast("Wrong password", "error");
            return;
          }
          renderShareLanding(token);
        } catch {
          toast("Wrong password", "error");
        }
      };
      input.addEventListener("keydown", (e) => {
        if (e.key === "Enter") submit();
      });
      body.append(
        el("label", { class: "field" }, [el("span", { class: "micro", text: "Password" }), input]),
        el("button", { class: "btn primary", text: "Continue", onclick: submit }),
      );
    } else if (info.requires_email) {
      head.append(el("div", { class: "meta", text: "This link is limited to specific addresses." }));

      const email = el("input", { type: "email", placeholder: "you@example.com" });
      const code = el("input", { type: "text", placeholder: "6-digit code from the email" });

      body.append(
        el("label", { class: "field" }, [el("span", { class: "micro", text: "Your email" }), email]),
        el("button", {
          class: "btn",
          text: "Send me a code",
          style: "width:100%;margin-bottom:14px",
          onclick: async () => {
            try {
              await fetch(`/s/${encodeURIComponent(token)}/recipient`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ email: email.value.trim() }),
              });
              toast("If that address was invited, a code is on its way");
            } catch {
              toast("Could not send a code", "error");
            }
          },
        }),
        el("label", { class: "field" }, [el("span", { class: "micro", text: "Code" }), code]),
        el("button", {
          class: "btn primary",
          text: "Open",
          onclick: async () => {
            try {
              const resp = await fetch(`/s/${encodeURIComponent(token)}/recipient/verify`, {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ email: email.value.trim(), code: code.value.trim() }),
              });
              if (!resp.ok) {
                toast("That code was not accepted", "error");
                return;
              }
              renderShareLanding(token);
            } catch {
              toast("That code was not accepted", "error");
            }
          },
        }),
      );
    } else if (info.kind === "request") {
      head.append(
        el("div", {
          class: "meta",
          text: info.max_uploads
            ? `Upload a file. ${Math.max(0, info.max_uploads - info.upload_count)} of ${info.max_uploads} slots left.`
            : "Upload a file to send it.",
        }),
      );

      const input = el("input", { type: "file" });
      const button = el("button", {
        class: "btn primary",
        text: "Upload",
        onclick: async () => {
          if (!input.files.length) return;
          const form = new FormData();
          form.append("file", input.files[0]);
          button.disabled = true;
          try {
            const resp = await fetch(`/s/${encodeURIComponent(token)}/upload`, { method: "POST", body: form });
            const payload = await resp.json().catch(() => ({}));
            if (!resp.ok) throw new Error(payload.message || "upload failed");
            toast(`Sent ${payload.name || "file"}`);
            renderShareLanding(token);
          } catch (err) {
            toast(err.message, "error");
            button.disabled = false;
          }
        },
      });

      body.append(el("label", { class: "field" }, [el("span", { class: "micro", text: "File" }), input]), button);
    } else if (!info.can_read) {
      body.append(el("p", { class: "hint", text: "This link cannot be downloaded." }));
    } else {
      body.append(
        el("button", {
          class: "btn primary",
          text: "Download",
          onclick: () => {
            location.href = `/s/${encodeURIComponent(token)}/download`;
          },
        }),
      );

      if (info.children && info.children.length > 0) {
        const list = el("div", { class: "share-list" });
        for (const child of info.children) {
          const row = el("div", { class: "row" }, [
            el("span", { class: "name", text: child.name }),
            el("span", { class: "size", text: child.kind === "dir" ? "" : humanSize(child.size) }),
          ]);
          if (child.kind !== "dir") {
            row.append(
              el("a", {
                href: `/s/${encodeURIComponent(token)}/download?name=${encodeURIComponent(child.name)}`,
                text: "Download",
                style: "font-size:11.5px",
              }),
            );
          }
          list.append(row);
        }
        body.append(list);
      }
    }

    card.append(body);
    card.append(
      el("div", { class: "foot" }, [
        el("span", { text: "newcloud" }),
        el("span", { text: info.expires_at ? `expires ${new Date(info.expires_at).toLocaleDateString()}` : "" }),
      ]),
    );
    root.append(el("div", { class: "share-page" }, [card]));
  }

  // ---------- routing ----------

  function switchTo(view) {
    state.view = view;
    state.entries = [];
    state.filter = "";
    state.selected.clear();
    state.anchor = null;
    state.detail = { path: null, versions: null, shares: null, loading: false };
    renderRail();
    renderContent();
    renderInspector();

    switch (view) {
      case "shares":
        loadShares();
        break;
      case "trash":
        loadTrash();
        break;
      case "audit":
        reloadAudit();
        break;
      case "access":
        loadCredentials();
        break;
      default:
        loadFiles();
    }
  }

  // renderMain redraws the content column from state without re-fetching. Selection and the
  // filter use it, and it must not reset scroll by hitting the network.
  function renderMain() {
    renderContent();
  }

  // ---------- keyboard ----------

  // A file manager is used from the keyboard more than from the mouse, and the previous
  // version ignored it entirely. These are the bindings people expect.
  function installKeyboard() {
    document.addEventListener("keydown", (e) => {
      // Never steal a key from a text field or an open dialog.
      const tag = (e.target && e.target.tagName) || "";
      if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT") return;
      if (document.querySelector("dialog[open]")) return;
      if (state.view !== "files") return;

      const list = visibleEntries();
      if (list.length === 0) return;

      const current = inspectedEntry();
      const index = current ? list.findIndex((x) => filePath(x) === filePath(current)) : -1;

      const focus = (i) => {
        if (i < 0 || i >= list.length) return;
        selectOnly(list[i]);
        renderContent();
        renderInspector();
        loadDetailIfNeeded();
        scrollIntoView(list[i]);
      };

      switch (e.key) {
        case "ArrowDown":
          e.preventDefault();
          focus(index < 0 ? 0 : Math.min(index + 1, list.length - 1));
          break;
        case "ArrowUp":
          e.preventDefault();
          focus(index < 0 ? list.length - 1 : Math.max(index - 1, 0));
          break;
        case "Home":
          e.preventDefault();
          focus(0);
          break;
        case "End":
          e.preventDefault();
          focus(list.length - 1);
          break;
        case "Enter":
          e.preventDefault();
          if (current) openEntry(current);
          break;
        case "Backspace":
          e.preventDefault();
          if (state.path) navigate(state.path.split("/").slice(0, -1).join("/"));
          break;
        case "Escape":
          if (state.selected.size > 0) {
            e.preventDefault();
            clearSelection();
          }
          break;
        case "a":
          if (e.metaKey || e.ctrlKey) {
            e.preventDefault();
            selectAll();
          }
          break;
        case "Delete":
          if (current) {
            e.preventDefault();
            confirmDelete(current);
          }
          break;
        case "F2":
          if (current) {
            e.preventDefault();
            promptRename(current);
          }
          break;
        case " ":
          if (current) {
            e.preventDefault();
            if (current.preview) openPreview(current);
          }
          break;
      }
    });
  }

  function scrollIntoView(entry) {
    const path = filePath(entry);
    const node = document.querySelector(`tr[data-path="${CSS.escape(path)}"]`);
    if (node && node.scrollIntoView) node.scrollIntoView({ block: "nearest" });
  }

  // ---------- drag and drop ----------

  // Drag and drop uploads to the current folder from anywhere on the page.
  //
  // The depth counter is necessary: dragleave fires when the pointer crosses into a child
  // element, so a naive listener flickers the overlay off the moment the pointer moves over
  // the table. Counting enters and leaves keeps it steady until the pointer leaves the
  // window.
  function installDragAndDrop() {
    const zone = $("#dropzone");
    const target = $("#drop-target");
    let depth = 0;

    const carriesFiles = (e) =>
      e.dataTransfer && Array.from(e.dataTransfer.types || []).includes("Files");

    window.addEventListener("dragenter", (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      depth++;
      if (state.view !== "files") return;
      if (target) target.textContent = state.path ? `into /${state.path}` : "into your home folder";
      zone.classList.add("active");
    });

    window.addEventListener("dragover", (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = "copy";
    });

    window.addEventListener("dragleave", (e) => {
      if (!carriesFiles(e)) return;
      depth--;
      if (depth <= 0) {
        depth = 0;
        zone.classList.remove("active");
      }
    });

    window.addEventListener("drop", (e) => {
      if (!carriesFiles(e)) return;
      e.preventDefault();
      depth = 0;
      zone.classList.remove("active");
      if (state.view !== "files") return;
      if (e.dataTransfer.files && e.dataTransfer.files.length) {
        uploadFiles(e.dataTransfer.files);
      }
    });
  }

  // ---------- boot ----------

  async function boot() {
    const shareMatch = location.pathname.match(/^\/s\/([^/]+)\/?$/);
    if (shareMatch) {
      await renderShareLanding(shareMatch[1]);
      return;
    }

    try {
      state.me = await api("GET", "/auth/me");
    } catch (err) {
      if (err instanceof APIError && err.status === 401) {
        location.href = "/api/auth/login?next=" + encodeURIComponent(location.pathname);
        return;
      }
      throw err;
    }

    installDragAndDrop();
    installKeyboard();
    renderRail();
    renderContent();
    renderInspector();

    const params = new URLSearchParams(location.search);
    if (params.get("path")) state.path = params.get("path").replace(/^\/+|\/+$/g, "");

    await loadFiles();
  }

  document.addEventListener("DOMContentLoaded", boot);
})();
