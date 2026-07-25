// newcloud — browser client
//
// Deliberately dependency-free: no framework, no bundler, no npm tree. The whole
// application is three static files embedded in the binary, so the shipped artifact
// contains exactly the code that was reviewed, with no transitive dependencies to audit
// and no build step to reproduce.
//
// Two rules hold everywhere in this file and are worth stating once:
//
//  1. User data reaches the DOM through textContent, never innerHTML. File names, share
//     labels and addresses come from other people; injecting one into markup is stored
//     XSS on the session of whoever browses the directory.
//  2. The server decides what may be previewed. The client never infers "this is an
//     image" from an extension — it reads the `preview` field the API returns, and the
//     download endpoint re-checks the type and the bytes regardless.

(() => {
  "use strict";

  // ---------- state ----------

  const state = {
    me: null,
    path: "",
    entries: [],
    view: "files", // files | shares | trash | credentials
    layout: "list", // list | grid
    filter: "",
  };

  const $ = (sel) => document.querySelector(sel);

  // ---------- DOM helper ----------

  // el builds an element. Text always goes through textContent.
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

  // icon returns an inline SVG. Icons are drawn here rather than loaded as a font or a
  // sprite because the CSP allows no external resources and the set is small.
  function icon(name, size = 16) {
    const paths = {
      search: "M11 11l3.5 3.5M7.5 12.5a5 5 0 100-10 5 5 0 000 10z",
      files: "M3 4.5A1.5 1.5 0 014.5 3h2.9l1.3 1.6H13A1.5 1.5 0 0114.5 6v7A1.5 1.5 0 0113 14.5H4.5A1.5 1.5 0 013 13V4.5z",
      shares: "M6.5 9.5l4-3M6.5 7.5l4 3M5 10a2 2 0 110-4 2 2 0 010 4zm7 5a2 2 0 110-4 2 2 0 010 4zm0-8a2 2 0 110-4 2 2 0 010 4z",
      trash: "M4 5.5h9M6.5 5.5V4a1 1 0 011-1h2a1 1 0 011 1v1.5M5.5 5.5l.5 8a1 1 0 001 1h3a1 1 0 001-1l.5-8",
      key: "M9.5 6.5a2.5 2.5 0 11-1.8 4.2L4 14.5H2.5V13l3.8-3.7A2.5 2.5 0 019.5 6.5z",
      upload: "M8 11V3m0 0L5 6m3-3l3 3M3 11v2a1 1 0 001 1h8a1 1 0 001-1v-2",
      folderPlus: "M3 4.5A1.5 1.5 0 014.5 3h2.9l1.3 1.6H13A1.5 1.5 0 0114.5 6v7A1.5 1.5 0 0113 14.5H4.5A1.5 1.5 0 013 13V4.5zM8 7.5v4M6 9.5h4",
      refresh: "M13.5 8a5.5 5.5 0 11-1.6-3.9M13.5 2v3h-3",
      list: "M3 4.5h10M3 8h10M3 11.5h10",
      grid: "M3 3.5h4v4H3zM9 3.5h4v4H9zM3 8.5h4v4H3zM9 8.5h4v4H9z",
      download: "M8 3v8m0 0l3-3m-3 3L5 8M3 13h10",
      close: "M4 4l8 8M12 4l-8 8",
      eye: "M1.5 8s2.4-4 6.5-4 6.5 4 6.5 4-2.4 4-6.5 4S1.5 8 1.5 8zM8 10a2 2 0 100-4 2 2 0 000 4z",
      image: "M3 4.5h10v7H3zM3 9.5l3-2.5 3 2.5 2-1.5 2 1.5M10 6.5a.75.75 0 100-1.5.75.75 0 000 1.5z",
      video: "M3 5.5h7v5H3zM10 7.5l3-1.5v4l-3-1.5",
      audio: "M9 3v6.5M9 9.5a1.75 1.75 0 11-3.5 0 1.75 1.75 0 013.5 0zM9 3l3 1",
      doc: "M4 2.5h5l3 3v8H4zM9 2.5v3h3",
    };
    const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
    svg.setAttribute("width", size);
    svg.setAttribute("height", size);
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("fill", "none");
    svg.setAttribute("stroke", "currentColor");
    svg.setAttribute("stroke-width", "1.4");
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

    if (resp.status === 401) {
      throw new APIError(401, "unauthenticated", "not signed in");
    }
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
    node.append(el("span", { class: "dot" }), el("span", { text: message }));
    node.className = "show" + (kind === "error" ? " error" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => {
      node.className = "";
    }, kind === "error" ? 6000 : 2800);
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
    if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
    if (seconds < 86400) return `${Math.floor(seconds / 3600)} h ago`;
    if (seconds < 86400 * 7) return `${Math.floor(seconds / 86400)} d ago`;
    return then.toLocaleDateString();
  }

  function joinPath(dir, name) {
    return dir ? `${dir}/${name}` : name;
  }

  function extensionOf(name) {
    const idx = name.lastIndexOf(".");
    if (idx <= 0 || idx === name.length - 1) return "";
    return name.slice(idx + 1);
  }

  // typeClass maps a node to the CSS class of its badge.
  //
  // It reads the server's `preview` field first: the client must not decide on its own
  // that a file is an image, because the server is the thing that will refuse to render
  // it inline and the two views have to agree.
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
    // A folder gets an icon, not a badge: it has no extension, and the literal "FILE" on a
    // directory is simply wrong.
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

  function isImage(entry) {
    return entry.kind !== "dir" && entry.preview === "image";
  }

  // previewURL is the inline endpoint. The server re-checks the type and sniffs the bytes,
  // so this URL is a request, not a permission.
  function previewURL(entry) {
    const path = entry.path || joinPath(state.path, entry.name);
    return `/api/files/download?inline=1&path=${encodeURIComponent(path)}`;
  }

  function downloadURL(entry) {
    const path = entry.path || joinPath(state.path, entry.name);
    return `/api/files/download?path=${encodeURIComponent(path)}`;
  }

  // ---------- chrome: sidebar ----------

  const NAV = [
    { key: "files", label: "Files", icon: "files", load: () => loadFiles() },
    { key: "shares", label: "Shares", icon: "shares", load: () => loadShares() },
    { key: "trash", label: "Trash", icon: "trash", load: () => loadTrash() },
    { key: "credentials", label: "App passwords", icon: "key", load: () => loadCredentials() },
  ];

  function renderSidebar() {
    const bar = $("#sidebar");
    clear(bar);

    bar.append(
      el("div", { class: "logo" }, [
        el("span", { class: "mark" }),
        el("span", { class: "word", text: "newcloud" }),
      ]),
    );

    const tabs = el("nav", { class: "tabs" });
    for (const item of NAV) {
      tabs.append(
        el(
          "button",
          {
            "aria-current": state.view === item.key ? "page" : null,
            onclick: () => switchTo(item.key),
          },
          [el("span", { class: "icon" }, [icon(item.icon)]), el("span", { text: item.label })],
        ),
      );
    }
    bar.append(tabs);

    bar.append(
      el("div", { class: "sidebar-footer" }, [
        el("div", { text: state.me ? state.me.username : "" }),
      ]),
    );
  }

  // ---------- chrome: header ----------

  function renderHeader() {
    const bar = $("#bar");
    clear(bar);

    const search = el("input", {
      type: "search",
      placeholder: "Search this folder",
      value: state.filter,
      "aria-label": "Search this folder",
      oninput: (e) => {
        state.filter = e.target.value;
        renderCurrentView();
      },
    });

    bar.append(
      el("div", { class: "search" }, [search, el("span", { class: "icon" }, [icon("search", 15)])]),
    );

    bar.append(el("span", { class: "spacer" }));

    if (state.me) {
      const initial = (state.me.username || "?").slice(0, 1);
      bar.append(
        el("div", { class: "account" }, [
          el("span", { class: "avatar", text: initial }),
          el("span", { class: "who", text: state.me.username }),
          el(
            "button",
            {
              class: "btn ghost",
              onclick: async () => {
                try {
                  await api("POST", "/auth/logout");
                } catch {
                  /* the cookie is gone either way */
                }
                location.href = "/api/auth/login";
              },
            },
            ["Sign out"],
          ),
        ]),
      );
    }
  }

  // ---------- files ----------

  function visibleEntries() {
    const q = state.filter.trim().toLowerCase();
    if (!q) return state.entries;
    return state.entries.filter((e) => e.name.toLowerCase().includes(q));
  }

  function renderCrumbs() {
    const crumbs = el("div", { class: "crumbs" });
    crumbs.append(
      el("button", {
        text: "Home",
        onclick: () => {
          state.path = "";
          state.filter = "";
          loadFiles();
        },
      }),
    );

    if (!state.path) return crumbs;

    let acc = "";
    for (const seg of state.path.split("/")) {
      acc = acc ? `${acc}/${seg}` : seg;
      const target = acc;
      crumbs.append(el("span", { class: "sep", text: "/" }));
      crumbs.append(
        el("button", {
          text: seg,
          onclick: () => {
            state.path = target;
            state.filter = "";
            loadFiles();
          },
        }),
      );
    }
    return crumbs;
  }

  function renderFiles() {
    const main = $("#main");
    clear(main);

    main.append(renderCrumbs());

    const fileInput = el("input", {
      type: "file",
      multiple: true,
      class: "hidden",
      onchange: (e) => uploadFiles(e.target.files),
    });

    const head = el("div", { class: "page-head" }, [
      el("button", { class: "btn primary", onclick: () => fileInput.click() }, [
        icon("upload"),
        el("span", { text: "Upload" }),
      ]),
      el("button", { class: "btn", onclick: () => promptNewFolder() }, [
        icon("folderPlus"),
        el("span", { text: "New folder" }),
      ]),
      fileInput,
      el("span", { class: "spacer" }),
      el(
        "button",
        {
          class: "btn ghost icon-only",
          title: state.layout === "list" ? "Switch to grid" : "Switch to list",
          "aria-label": state.layout === "list" ? "Switch to grid" : "Switch to list",
          onclick: () => {
            state.layout = state.layout === "list" ? "grid" : "list";
            renderCurrentView();
            renderHeader();
          },
        },
        [icon(state.layout === "list" ? "grid" : "list")],
      ),
      el(
        "button",
        {
          class: "btn ghost icon-only",
          title: "Refresh",
          "aria-label": "Refresh",
          onclick: () => loadFiles(),
        },
        [icon("refresh")],
      ),
    ]);
    main.append(head);

    const progress = el("div", { class: "progress hidden" }, [el("div")]);
    main.append(progress);

    const entries = visibleEntries();

    if (entries.length === 0) {
      main.append(
        el("div", { class: "empty" }, [
          el("div", { class: "glyph" }, [icon(state.filter ? "search" : "files", 32)]),
          el("div", {
            class: "title",
            text: state.filter ? "Nothing matches" : "This folder is empty",
          }),
          el("div", {
            class: "hint",
            text: state.filter ? "Try a different search." : "Drop files here, or use Upload.",
          }),
        ]),
      );
      return;
    }

    if (state.layout === "grid") {
      main.append(renderGrid(entries));
    } else {
      main.append(renderTable(entries));
    }
  }

  function renderTable(entries) {
    const table = el("table", { class: "files" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { text: "Name" }),
          el("th", { class: "num", text: "Size" }),
          el("th", { text: "Modified" }),
          el("th", { text: "" }),
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

    const glyph = isImage(entry)
      ? el("img", {
          class: "thumb",
          src: previewURL(entry),
          alt: "",
          loading: "lazy",
          // A broken thumbnail must not leave a hole: fall back to the badge.
          onerror: (e) => {
            const span = el("span", { class: `ftype ${typeClass(entry)}`, text: badgeGlyph(entry) });
            e.target.replaceWith(span);
          },
        })
      : el("span", { class: `ftype ${typeClass(entry)}` },
          isDir ? [icon("files", 15)] : [badgeGlyph(entry)]);

    const label = el("span", {
      class: "label" + (isDir ? " dir" : ""),
      text: entry.name,
      title: entry.name,
      onclick: () => openEntry(entry),
    });

    const actions = el("td", { class: "actions" });
    if (!isDir) {
      if (entry.preview) {
        actions.append(
          el("button", {
            class: "row-action",
            text: "Preview",
            onclick: () => openPreview(entry),
          }),
        );
      }
      actions.append(
        el("button", { class: "row-action", text: "Download", onclick: () => downloadFile(entry) }),
      );
    }
    actions.append(el("button", { class: "row-action", text: "Share", onclick: () => openShareDialog(entry) }));
    actions.append(el("button", { class: "row-action", text: "Rename", onclick: () => promptRename(entry) }));
    actions.append(
      el("button", {
        class: "row-action danger",
        text: "Delete",
        onclick: () => confirmDelete(entry),
      }),
    );

    return el("tr", {}, [
      el("td", { class: "name" }, [glyph, label]),
      el("td", { class: "size", text: isDir ? "—" : humanSize(entry.size) }),
      el("td", { class: "when", text: humanTime(entry.modified_at) }),
      actions,
    ]);
  }

  function renderGrid(entries) {
    const grid = el("div", { class: "grid" });
    for (const entry of entries) {
      const isDir = entry.kind === "dir";

      let preview;
      if (isDir) {
        preview = el("div", { class: "preview dir" }, [el("span", { class: "glyph" }, [icon("files", 30)])]);
      } else if (isImage(entry)) {
        preview = el("div", { class: "preview" }, [
          el("img", {
            src: previewURL(entry),
            alt: "",
            loading: "lazy",
            onerror: (e) => {
              e.target.replaceWith(el("span", { class: "glyph" }, [icon("doc", 30)]));
            },
          }),
        ]);
      } else {
        const glyphName =
          entry.preview === "video" ? "video" : entry.preview === "audio" ? "audio" : "doc";
        preview = el("div", { class: "preview" }, [el("span", { class: "glyph" }, [icon(glyphName, 30)])]);
      }

      grid.append(
        el("div", {
          class: "tile",
          title: entry.name,
          onclick: () => openEntry(entry),
          oncontextmenu: (e) => {
            e.preventDefault();
            openShareDialog(entry);
          },
        }, [
          preview,
          el("div", { class: "meta" }, [
            el("div", { class: "n", text: entry.name }),
            el("div", { class: "s", text: isDir ? "Folder" : humanSize(entry.size) }),
          ]),
        ]),
      );
    }
    return grid;
  }

  // openEntry is the click behaviour: folders navigate, previewable files preview,
  // everything else downloads. That is what a person expects from a file manager.
  function openEntry(entry) {
    if (entry.kind === "dir") {
      state.path = joinPath(state.path, entry.name);
      state.filter = "";
      loadFiles();
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
      renderCurrentView();
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

  // ---------- preview ----------

  let activeDialog = null;

  // openPreview renders a file in a modal.
  //
  // Each branch builds a specific element with a known-safe attribute set. Nothing here
  // interpolates a filename into markup; the title is textContent, and the URL is the API
  // endpoint, which decides for itself whether to render inline.
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
              body.append(unsupported(entry, "This image could not be displayed."));
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
              body.append(unsupported(entry, "This video format is not supported by the browser."));
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
              body.append(unsupported(entry, "This audio format is not supported by the browser."));
            },
          }),
        );
        break;

      case "pdf":
        // The browser's own viewer, in a plain frame.
        //
        // No `sandbox` attribute: Chrome refuses to instantiate its PDF plugin inside a
        // sandboxed frame, and the file then renders as a broken-page icon. The response
        // already carries `sandbox` in its CSP, which is the layer that matters — it applies
        // to whatever the frame loads, and this frame loads exactly one URL on our origin.
        body.append(el("iframe", { src: url, title: entry.name }));
        break;

      case "text":
        body.append(el("pre", { text: "Loading…" }));
        loadTextPreview(url, entry, body);
        break;

      default:
        body.append(unsupported(entry, "No preview for this file type."));
    }

    const dialog = el("dialog", { class: "preview" }, [
      el("div", { class: "preview-head" }, [
        el("span", { class: `ftype ${typeClass(entry)}`, text: badgeGlyph(entry) }),
        el("span", { class: "title", text: entry.name, title: entry.name }),
        el("span", { class: "sub", text: humanSize(entry.size) }),
        el("span", { class: "spacer" }),
        el("button", { class: "btn ghost", onclick: () => downloadFile(entry) }, [
          icon("download"),
          el("span", { text: "Download" }),
        ]),
        el(
          "button",
          {
            class: "btn ghost icon-only",
            "aria-label": "Close",
            title: "Close",
            onclick: () => dialog.close(),
          },
          [icon("close")],
        ),
      ]),
      body,
    ]);

    showDialog(dialog);
  }

  // unsupported is the "cannot render this" panel.
  function unsupported(entry, message) {
    return el("div", { class: "unsupported" }, [
      el("strong", { text: "Preview unavailable" }),
      el("span", { text: message }),
    ]);
  }

  // loadTextPreview fetches the bytes and inserts them as text.
  //
  // textContent, not innerHTML: a .html file previewed here is shown as source, and if it
  // were parsed the browser would execute it. The server also refuses to inline HTML, so
  // this is the second of two independent defences.
  async function loadTextPreview(url, entry, container) {
    try {
      const resp = await fetch(url, { credentials: "same-origin" });
      if (!resp.ok) throw new Error(`HTTP ${resp.status}`);
      const text = await resp.text();
      clear(container);
      container.append(el("pre", { text: text }));
    } catch (err) {
      clear(container);
      container.append(unsupported(entry, `Could not load the file: ${err.message}`));
    }
  }

  // ---------- uploads ----------

  async function uploadFiles(fileList) {
    if (!fileList || fileList.length === 0) return;
    const progress = $("#main .progress");
    const bar = progress && progress.firstElementChild;
    const total = fileList.length;
    let done = 0;

    for (const file of fileList) {
      const form = new FormData();
      form.append("path", state.path);
      form.append("file", file);

      try {
        if (progress) progress.classList.remove("hidden");
        if (bar) bar.style.width = `${Math.round((done / total) * 100)}%`;

        await api("POST", "/files/upload", form);
        done++;
        if (bar) bar.style.width = `${Math.round((done / total) * 100)}%`;
      } catch (err) {
        fail(err);
        break;
      }
    }

    if (done > 0) {
      toast(done === 1 ? `Uploaded ${done} file` : `Uploaded ${done} files`);
    }
    setTimeout(() => {
      if (progress) progress.classList.add("hidden");
      if (bar) bar.style.width = "0";
    }, 400);

    await loadFiles();
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

  // formDialog builds a modal with fields and a submit handler.
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
      bodyWrap.append(el("label", { class: "field" }, [el("span", { text: field.label }), input]));
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
      fields: [{ name: "name", label: "Folder name", type: "text" }],
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
        await api("POST", "/files/rename", {
          path: joinPath(state.path, entry.name),
          name: v.name,
        });
        toast("Renamed");
        await loadFiles();
      },
    });
  }

  function confirmDelete(entry) {
    formDialog({
      title: `Delete ${entry.name}?`,
      body: "It moves to the trash and can be restored from there.",
      fields: [],
      confirm: "Delete",
      danger: true,
      onSubmit: async () => {
        await api("DELETE", `/files/?path=${encodeURIComponent(joinPath(state.path, entry.name))}`);
        toast("Moved to trash");
        await loadFiles();
      },
    });
  }

  function openShareDialog(entry) {
    const path = joinPath(state.path, entry.name);
    const isDir = entry.kind === "dir";

    const kindField = {
      name: "kind",
      label: "Type",
      type: "select",
      options: [
        { value: "link", label: "Link — anyone with the URL can download" },
        ...(isDir ? [{ value: "request", label: "Request — others can upload into this folder" }] : []),
      ],
    };

    formDialog({
      title: `Share ${entry.name}`,
      fields: [
        kindField,
        { name: "password", label: "Password (optional)", type: "password" },
        { name: "expires_hours", label: "Expires in hours (optional)", type: "number", min: "0" },
        { name: "max_uses", label: "Maximum downloads (optional)", type: "number", min: "0" },
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
            extra: el("div", { class: "mono", text: url }),
            onSubmit: async () => {},
          });
        } else {
          toast("Share created");
        }
        await loadFiles();
      },
    });
  }

  // ---------- shares ----------

  function renderShares() {
    const main = $("#main");
    clear(main);

    main.append(
      el("div", { class: "page-head" }, [el("h1", { class: "page-title", text: "Shares" })]),
    );
    main.append(
      el("p", {
        class: "hint",
        text: "Links you have created. A password is never shown again — only whether one is set.",
        style: "color:var(--text-secondary);margin:0 0 16px;font-size:13px",
      }),
    );

    if (state.entries.length === 0) {
      main.append(
        el("div", { class: "empty" }, [
          el("div", { class: "glyph" }, [icon("shares", 32)]),
          el("div", { class: "title", text: "No shares yet" }),
          el("div", { class: "hint", text: "Open a file and choose Share to create a link." }),
        ]),
      );
      return;
    }

    const table = el("table", { class: "files" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { text: "Target" }),
          el("th", { text: "Type" }),
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

      const actions = el("td", { class: "actions" });
      actions.append(
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
      );

      body.append(
        el("tr", {}, [
          el("td", { text: share.node_path || share.label || "—" }),
          el("td", { text: share.kind }),
          el("td", { class: "size", text: uses }),
          el("td", {
            class: "when",
            text: share.expires_at ? new Date(share.expires_at).toLocaleString() : "never",
          }),
          actions,
        ]),
      );
    }
    table.append(body);
    main.append(el("div", { class: "table-wrap" }, [table]));
  }

  async function loadShares() {
    try {
      const data = await api("GET", "/shares/");
      state.entries = data.items || [];
      renderCurrentView();
    } catch (err) {
      fail(err);
    }
  }

  // ---------- trash ----------

  function renderTrash() {
    const main = $("#main");
    clear(main);

    main.append(el("div", { class: "page-head" }, [el("h1", { class: "page-title", text: "Trash" })]));

    if (state.entries.length === 0) {
      main.append(
        el("div", { class: "empty" }, [
          el("div", { class: "glyph" }, [icon("trash", 32)]),
          el("div", { class: "title", text: "Trash is empty" }),
          el("div", { class: "hint", text: "Deleted files appear here and can be restored." }),
        ]),
      );
      return;
    }

    const table = el("table", { class: "files" });
    table.append(
      el("thead", {}, [
        el("tr", {}, [
          el("th", { text: "Name" }),
          el("th", { class: "num", text: "Size" }),
          el("th", { text: "" }),
        ]),
      ]),
    );
    const body = el("tbody");
    for (const entry of state.entries) {
      body.append(
        el("tr", {}, [
          el("td", { class: "name" }, [
            el("span", { class: `ftype ${typeClass(entry)}`, text: badgeGlyph(entry) }),
            el("span", { class: "label", text: entry.name }),
          ]),
          el("td", { class: "size", text: entry.kind === "dir" ? "—" : humanSize(entry.size) }),
          el("td", { class: "actions" }, [
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
    main.append(el("div", { class: "table-wrap" }, [table]));
  }

  async function loadTrash() {
    try {
      const data = await api("GET", "/files/trash");
      state.entries = data.items || [];
      renderCurrentView();
    } catch (err) {
      fail(err);
    }
  }

  // ---------- app passwords ----------

  function renderCredentials() {
    const main = $("#main");
    clear(main);

    main.append(
      el("div", { class: "page-head" }, [el("h1", { class: "page-title", text: "App passwords" })]),
    );

    main.append(
      el("div", { class: "card" }, [
        el("h2", { text: "WebDAV access" }),
        el("p", {
          class: "hint",
          text:
            "Use an app password instead of your browser login when connecting a WebDAV client, " +
            "rclone, or a mounted drive. Each one can be revoked on its own.",
        }),
        el("button", { class: "btn primary", onclick: () => createAppPassword() }, [
          icon("key"),
          el("span", { text: "New app password" }),
        ]),
      ]),
    );

    if (state.entries.length === 0) return;

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
          el("td", { text: cred.label || "—" }),
          el("td", { text: cred.scope }),
          el("td", { class: "when", text: cred.last_used ? humanTime(cred.last_used) : "never" }),
          el("td", { class: "actions" }, [
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

    main.append(el("div", { class: "table-wrap", style: "margin-top:16px" }, [table]));
  }

  async function loadCredentials() {
    try {
      const data = await api("GET", "/apppasswords/");
      state.entries = data.items || [];
      renderCurrentView();
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
          extra: el("div", { class: "mono", text: created.secret || "" }),
          onSubmit: async () => {
            await loadCredentials();
          },
        });
      },
    });
  }

  // ---------- share landing page ----------

  async function renderShareLanding(token) {
    const root = $("#app");
    clear(root);
    // The landing page is its own screen: no sidebar, no header.
    //
    // .share-page positions itself fixed, which takes it out of the #app grid (that grid
    // exists for the signed-in shell and would otherwise give this page the sidebar width).
    // Nothing needs overriding here.

    let info;
    try {
      const resp = await fetch(`/s/${encodeURIComponent(token)}`, {
        headers: { Accept: "application/json" },
      });
      if (!resp.ok) throw new Error("not available");
      info = await resp.json();
    } catch {
      root.append(
        el("div", { class: "share-page" }, [
          el("div", { class: "share-card" }, [
            el("div", { class: "head" }, [
              el("div", { class: "mark" }),
              el("div", { class: "title", text: "Link not available" }),
              el("div", {
                class: "meta",
                text: "It may have expired, been revoked, or reached its usage limit.",
              }),
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
        el("label", { class: "field" }, [el("span", { text: "Password" }), input]),
        el("button", { class: "btn primary", text: "Continue", onclick: submit, style: "width:100%" }),
      );
    } else if (info.requires_email) {
      head.append(el("div", { class: "meta", text: "This link is limited to specific addresses." }));

      const email = el("input", { type: "email", placeholder: "you@example.com" });
      const code = el("input", { type: "text", placeholder: "6-digit code from the email" });

      body.append(
        el("label", { class: "field" }, [el("span", { text: "Your email" }), email]),
        el(
          "button",
          {
            class: "btn",
            text: "Send me a code",
            style: "width:100%;margin-bottom:16px",
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
          },
        ),
        el("label", { class: "field" }, [el("span", { text: "Code" }), code]),
        el("button", {
          class: "btn primary",
          text: "Open",
          style: "width:100%",
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
        style: "width:100%",
        onclick: async () => {
          if (!input.files.length) return;
          const form = new FormData();
          form.append("file", input.files[0]);
          button.disabled = true;
          try {
            const resp = await fetch(`/s/${encodeURIComponent(token)}/upload`, {
              method: "POST",
              body: form,
            });
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

      body.append(el("label", { class: "field" }, [el("span", { text: "File" }), input]), button);
    } else if (!info.can_read) {
      body.append(el("p", { class: "hint", text: "This link cannot be downloaded." }));
    } else {
      body.append(
        el("button", {
          class: "btn primary",
          text: "Download",
          style: "width:100%",
          onclick: () => {
            location.href = `/s/${encodeURIComponent(token)}/download`;
          },
        }),
      );

      if (info.children && info.children.length > 0) {
        const list = el("div", { class: "share-list" });
        for (const child of info.children) {
          const row = el("div", { class: "row" }, [
            el("span", { class: `ftype ${typeClass(child)}`, text: badgeGlyph(child) }),
            el("span", { class: "name", text: child.name }),
            el("span", {
              class: "size",
              text: child.kind === "dir" ? "" : humanSize(child.size),
            }),
          ]);
          if (child.kind !== "dir") {
            row.append(
              el("span", { class: "size" }, [
                el("a", {
                  href: `/s/${encodeURIComponent(token)}/download?name=${encodeURIComponent(child.name)}`,
                  text: "Download",
                }),
              ]),
            );
          }
          list.append(row);
        }
        body.append(list);
      }
    }

    card.append(body);
    card.append(el("div", { class: "foot", text: "Shared with newcloud" }));
    root.append(el("div", { class: "share-page" }, [card]));
  }

  // ---------- routing ----------

  function switchTo(view) {
    state.view = view;
    state.entries = [];
    state.filter = "";
    renderSidebar();
    renderHeader();

    switch (view) {
      case "shares":
        loadShares();
        break;
      case "trash":
        loadTrash();
        break;
      case "credentials":
        loadCredentials();
        break;
      default:
        loadFiles();
    }
  }

  // renderCurrentView re-renders without re-fetching. Search and the list/grid toggle use
  // it, and it must not reset scroll position by hitting the network.
  function renderCurrentView() {
    renderSidebar();
    switch (state.view) {
      case "shares":
        renderShares();
        break;
      case "trash":
        renderTrash();
        break;
      case "credentials":
        renderCredentials();
        break;
      default:
        renderFiles();
    }
  }

  // ---------- drag and drop ----------

  function installDragAndDrop() {
    const zone = $("#dropzone");
    let depth = 0;

    window.addEventListener("dragenter", (e) => {
      if (!e.dataTransfer || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
      depth++;
      if (state.view === "files") zone.classList.add("active");
    });
    window.addEventListener("dragover", (e) => {
      if (!e.dataTransfer || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
    });
    window.addEventListener("dragleave", () => {
      if (--depth <= 0) {
        depth = 0;
        zone.classList.remove("active");
      }
    });
    window.addEventListener("drop", (e) => {
      if (!e.dataTransfer || !e.dataTransfer.types.includes("Files")) return;
      e.preventDefault();
      depth = 0;
      zone.classList.remove("active");
      if (state.view !== "files") return;
      if (e.dataTransfer.files.length) uploadFiles(e.dataTransfer.files);
    });
  }

  // ---------- boot ----------

  async function boot() {
    // A /s/<token> URL is the anonymous share page: no session, no shell.
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
    renderSidebar();
    renderHeader();

    // Honour a deep link such as /?path=docs.
    const params = new URLSearchParams(location.search);
    if (params.get("path")) state.path = params.get("path").replace(/^\/+|\/+$/g, "");

    await loadFiles();
  }

  document.addEventListener("DOMContentLoaded", boot);
})();
