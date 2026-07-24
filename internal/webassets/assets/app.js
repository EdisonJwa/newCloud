// newcloud frontend.
//
// Deliberately dependency-free: no framework, no bundler, no npm tree. The whole
// application is three static files embedded in the binary, which means the shipped
// artifact contains exactly the code that was reviewed, with no transitive
// dependencies to audit and no build step to reproduce.
//
// Every DOM node that carries user data (file names, share labels, error messages) is
// built with textContent, never innerHTML. File names come from other users, so
// injecting one into markup would be stored XSS on the session of whoever browses the
// directory.

(() => {
  "use strict";

  // ---------- state ----------

  const state = {
    me: null,
    path: "",          // current directory, "" is the root
    entries: [],
    view: "files",     // files | shares | trash | credentials
    shareToken: null,  // set when serving /s/<token>
  };

  const $ = (sel) => document.querySelector(sel);

  // ---------- tiny DOM helper ----------

  // el builds an element. Text is always set via textContent: passing a string to a
  // markup API is how XSS gets in, and there is no case here that needs it.
  function el(tag, opts = {}, children = []) {
    const node = document.createElement(tag);
    for (const [key, value] of Object.entries(opts)) {
      if (value == null) continue;
      if (key === "text") node.textContent = value;
      else if (key === "class") node.className = value;
      else if (key === "dataset") Object.assign(node.dataset, value);
      else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
      else node.setAttribute(key, value);
    }
    for (const child of [].concat(children)) {
      if (child == null) continue;
      node.append(typeof child === "string" ? document.createTextNode(child) : child);
    }
    return node;
  }

  function clear(node) {
    while (node.firstChild) node.removeChild(node.firstChild);
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
      // A 401 means the session is gone; sending the user to the login flow is the
      // only useful action. The share pages are anonymous and never reach here.
      throw new APIError(401, "unauthenticated", "not signed in");
    }
    if (resp.status === 204) return null;

    const text = await resp.text();
    let payload = null;
    if (text) {
      try { payload = JSON.parse(text); } catch { payload = { message: text }; }
    }

    if (!resp.ok) {
      throw new APIError(resp.status, payload?.error, payload?.message);
    }
    return payload;
  }

  // ---------- toast ----------

  let toastTimer = null;
  function toast(message, kind = "info") {
    const node = $("#toast");
    node.textContent = message;
    node.className = "show" + (kind === "error" ? " error" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { node.className = ""; }, kind === "error" ? 6000 : 3000);
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
    if (bytes === 0) return "0 B";
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

  function parentPath(path) {
    const idx = path.lastIndexOf("/");
    return idx === -1 ? "" : path.slice(0, idx);
  }

  // ---------- chrome ----------

  function renderHeader() {
    const header = $("#bar");
    clear(header);

    header.append(el("span", { class: "brand", text: "newcloud" }));

    const tabs = el("nav", { class: "tabs" });
    for (const [key, label] of [
      ["files", "Files"],
      ["shares", "Shares"],
      ["trash", "Trash"],
      ["credentials", "App passwords"],
    ]) {
      tabs.append(el("button", {
        text: label,
        "aria-current": state.view === key ? "page" : null,
        onclick: () => switchTo(key),
      }));
    }
    header.append(tabs);
    header.append(el("span", { class: "spacer" }));

    if (state.me) {
      header.append(el("span", { class: "who", text: state.me.username }));
      header.append(el("button", {
        text: "Sign out",
        onclick: async () => {
          try { await api("POST", "/auth/logout"); } catch { /* ignore */ }
          location.href = "/api/auth/login";
        },
      }));
    }
  }

  // ---------- files view ----------

  function renderCrumbs() {
    const crumbs = el("div", { class: "crumbs" });
    crumbs.append(el("button", {
      class: "link",
      text: "Home",
      onclick: () => { state.path = ""; loadFiles(); },
    }));

    if (!state.path) return crumbs;

    let acc = "";
    for (const seg of state.path.split("/")) {
      acc = acc ? `${acc}/${seg}` : seg;
      const target = acc;
      crumbs.append(el("span", { class: "sep", text: "/" }));
      crumbs.append(el("button", {
        class: "link",
        text: seg,
        onclick: () => { state.path = target; loadFiles(); },
      }));
    }
    return crumbs;
  }

  function renderFiles() {
    const main = $("#main");
    clear(main);

    main.append(renderCrumbs());

    // Toolbar.
    const fileInput = el("input", {
      type: "file",
      multiple: "multiple",
      class: "hidden",
      onchange: (e) => uploadFiles(e.target.files),
    });

    const toolbar = el("div", { class: "toolbar" }, [
      el("button", {
        class: "primary",
        text: "Upload",
        onclick: () => fileInput.click(),
      }),
      el("button", {
        text: "New folder",
        onclick: () => promptNewFolder(),
      }),
      fileInput,
      el("span", { class: "spacer" }),
      el("button", {
        text: "Refresh",
        onclick: () => loadFiles(),
      }),
    ]);
    main.append(toolbar);

    const progress = el("div", { class: "progress hidden" }, [el("div")]);
    main.append(progress);

    // File table.
    if (state.entries.length === 0) {
      main.append(el("div", { class: "empty", text: "This folder is empty. Drop files here to upload." }));
      return;
    }

    const table = el("table", { class: "files" });
    table.append(el("thead", {}, [
      el("tr", {}, [
        el("th", { text: "Name" }),
        el("th", { text: "Size" }),
        el("th", { text: "Modified" }),
        el("th", { text: "" }),
      ]),
    ]));

    const body = el("tbody");
    for (const entry of state.entries) {
      const isDir = entry.kind === "dir";
      const nameCell = el("td", { class: "name" }, [
        el("span", { class: "icon", text: isDir ? "📁" : "📄" }),
        isDir
          ? el("button", {
              class: "link",
              text: entry.name,
              onclick: () => { state.path = joinPath(state.path, entry.name); loadFiles(); },
            })
          : el("span", { text: entry.name }),
      ]);

      const actions = el("td", { class: "actions" });
      if (!isDir) {
        actions.append(el("button", {
          class: "link",
          text: "Download",
          onclick: () => downloadFile(entry),
        }));
      }
      actions.append(el("button", {
        class: "link",
        text: "Share",
        onclick: () => openShareDialog(entry),
      }));
      actions.append(el("button", {
        class: "link",
        text: "Rename",
        onclick: () => promptRename(entry),
      }));
      actions.append(el("button", {
        class: "link danger",
        text: "Delete",
        onclick: () => confirmDelete(entry),
      }));

      body.append(el("tr", {}, [
        nameCell,
        el("td", { class: "size", text: isDir ? "—" : humanSize(entry.size) }),
        el("td", { class: "when", text: humanTime(entry.modified_at) }),
        actions,
      ]));
    }

    table.append(body);
    main.append(table);
  }

  async function loadFiles() {
    try {
      const data = await api("GET", `/files/?path=${encodeURIComponent(state.path)}`);
      state.entries = data.children || [];
      render();
    } catch (err) {
      fail(err);
    }
  }

  function downloadFile(entry) {
    const path = joinPath(state.path, entry.name);
    // A plain navigation, not fetch: it lets the browser stream to disk and use its
    // own download UI instead of buffering the file in memory.
    const link = el("a", { href: `/api/files/download?path=${encodeURIComponent(path)}`, download: entry.name });
    document.body.append(link);
    link.click();
    link.remove();
  }

  async function uploadFiles(fileList) {
    if (!fileList || fileList.length === 0) return;
    const progress = $("#main .progress");
    const bar = progress?.firstElementChild;

    for (const file of fileList) {
      const form = new FormData();
      form.append("path", state.path);
      form.append("file", file);

      try {
        progress?.classList.remove("hidden");
        if (bar) bar.style.width = "30%";

        await api("POST", "/files/upload", form);

        if (bar) bar.style.width = "100%";
        toast(`Uploaded ${file.name}`);
      } catch (err) {
        fail(err);
        break;
      } finally {
        setTimeout(() => {
          progress?.classList.add("hidden");
          if (bar) bar.style.width = "0";
        }, 500);
      }
    }
    await loadFiles();
  }

  function promptNewFolder() {
    openDialog({
      title: "New folder",
      fields: [{ name: "name", label: "Folder name", type: "text" }],
      confirm: "Create",
      onSubmit: async (values) => {
        await api("POST", "/files/mkdir", { path: joinPath(state.path, values.name) });
        toast("Folder created");
        await loadFiles();
      },
    });
  }

  function promptRename(entry) {
    openDialog({
      title: `Rename ${entry.name}`,
      fields: [{ name: "name", label: "New name", type: "text", value: entry.name }],
      confirm: "Rename",
      onSubmit: async (values) => {
        await api("POST", "/files/rename", {
          path: joinPath(state.path, entry.name),
          name: values.name,
        });
        toast("Renamed");
        await loadFiles();
      },
    });
  }

  function confirmDelete(entry) {
    openDialog({
      title: `Delete ${entry.name}?`,
      body: "It moves to the trash and can be restored from there.",
      confirm: "Delete",
      danger: true,
      onSubmit: async () => {
        await api("DELETE", `/files/?path=${encodeURIComponent(joinPath(state.path, entry.name))}`);
        toast("Moved to trash");
        await loadFiles();
      },
    });
  }

  // ---------- shares view ----------

  function renderShares() {
    const main = $("#main");
    clear(main);

    main.append(el("p", { class: "hint", text: "Links you have created. Passwords are never shown again; only whether one is set." }));

    if (state.entries.length === 0) {
      main.append(el("div", { class: "empty", text: "No shares yet. Open a file and choose Share." }));
      return;
    }

    const table = el("table", { class: "files" });
    table.append(el("thead", {}, [
      el("tr", {}, [
        el("th", { text: "Target" }),
        el("th", { text: "Kind" }),
        el("th", { text: "Uses" }),
        el("th", { text: "Expires" }),
        el("th", { text: "" }),
      ]),
    ]));

    const body = el("tbody");
    for (const share of state.entries) {
      const uses = share.kind === "request"
        ? `${share.upload_count}${share.max_uploads ? "/" + share.max_uploads : ""}`
        : `${share.download_count}${share.max_downloads ? "/" + share.max_downloads : ""}`;

      const actions = el("td", { class: "actions" });
      if (share.url) {
        actions.append(el("button", {
          class: "link",
          text: "Copy link",
          onclick: async () => {
            await navigator.clipboard.writeText(share.url);
            toast("Link copied");
          },
        }));
      }
      actions.append(el("button", {
        class: "link danger",
        text: "Revoke",
        onclick: async () => {
          try {
            await api("DELETE", `/shares/${share.id}`);
            toast("Share revoked");
            loadShares();
          } catch (err) { fail(err); }
        },
      }));

      body.append(el("tr", {}, [
        el("td", { text: share.node_path || share.label || "—" }),
        el("td", { text: share.kind }),
        el("td", { class: "size", text: uses }),
        el("td", { class: "when", text: share.expires_at ? new Date(share.expires_at).toLocaleString() : "never" }),
        actions,
      ]));
    }
    table.append(body);
    main.append(table);
  }

  async function loadShares() {
    try {
      const data = await api("GET", "/shares/");
      state.entries = data.items || [];
      render();
    } catch (err) { fail(err); }
  }

  function openShareDialog(entry) {
    const path = joinPath(state.path, entry.name);
    const isDir = entry.kind === "dir";

    openDialog({
      title: `Share ${entry.name}`,
      fields: [
        {
          name: "kind",
          label: "Type",
          type: "select",
          options: [
            { value: "link", label: "Link — anyone with the link can download" },
            ...(isDir ? [{ value: "request", label: "Request — others can upload into this folder" }] : []),
          ],
        },
        { name: "password", label: "Password (optional)", type: "password" },
        { name: "expires_in_seconds", label: "Expires in hours (optional)", type: "number", min: "0" },
        { name: "max_downloads", label: "Maximum downloads (optional)", type: "number", min: "0" },
      ],
      confirm: "Create link",
      onSubmit: async (values) => {
        const payload = { path, kind: values.kind, hide_contents: values.kind === "request" };
        if (values.password) payload.password = values.password;

        const hours = parseInt(values.expires_in_seconds, 10);
        if (hours > 0) payload.expires_in_seconds = hours * 3600;

        const max = parseInt(values.max_downloads, 10);
        if (max > 0) {
          if (values.kind === "request") payload.max_uploads = max;
          else payload.max_downloads = max;
        }

        const created = await api("POST", "/shares/", payload);
        const url = created.url || created.share?.url;
        if (url) {
          await navigator.clipboard.writeText(url).catch(() => {});
          toast("Link created and copied to the clipboard");
        } else {
          toast("Share created");
        }
        await loadFiles();
      },
    });
  }

  // ---------- trash view ----------

  function renderTrash() {
    const main = $("#main");
    clear(main);

    if (state.entries.length === 0) {
      main.append(el("div", { class: "empty", text: "Trash is empty." }));
      return;
    }

    const table = el("table", { class: "files" });
    table.append(el("thead", {}, [
      el("tr", {}, [el("th", { text: "Name" }), el("th", { text: "Size" }), el("th", { text: "" })]),
    ]));
    const body = el("tbody");
    for (const entry of state.entries) {
      body.append(el("tr", {}, [
        el("td", { class: "name" }, [
          el("span", { class: "icon", text: entry.kind === "dir" ? "📁" : "📄" }),
          el("span", { text: entry.name }),
        ]),
        el("td", { class: "size", text: entry.kind === "dir" ? "—" : humanSize(entry.size) }),
        el("td", { class: "actions" }, [
          el("button", {
            class: "link",
            text: "Restore",
            onclick: async () => {
              try {
                await api("POST", "/files/restore", { id: entry.id });
                toast("Restored");
                loadTrash();
              } catch (err) { fail(err); }
            },
          }),
        ]),
      ]));
    }
    table.append(body);
    main.append(table);
  }

  async function loadTrash() {
    try {
      const data = await api("GET", "/files/trash");
      state.entries = data.items || [];
      render();
    } catch (err) { fail(err); }
  }

  // ---------- app passwords ----------

  function renderCredentials() {
    const main = $("#main");
    clear(main);

    main.append(el("div", { class: "card" }, [
      el("h2", { text: "WebDAV / app passwords" }),
      el("p", {
        class: "hint",
        text: "Use one of these instead of your browser login when connecting a WebDAV client, " +
              "rclone or a mounted drive. Each password can be revoked on its own.",
      }),
      el("button", {
        class: "primary",
        text: "New app password",
        onclick: () => createAppPassword(),
      }),
    ]));

    if (state.entries.length === 0) {
      main.append(el("div", { class: "empty", text: "No app passwords yet." }));
      return;
    }

    const table = el("table", { class: "files" });
    table.append(el("thead", {}, [
      el("tr", {}, [
        el("th", { text: "Label" }),
        el("th", { text: "Scope" }),
        el("th", { text: "Last used" }),
        el("th", { text: "" }),
      ]),
    ]));
    const body = el("tbody");
    for (const cred of state.entries) {
      body.append(el("tr", {}, [
        el("td", { text: cred.label || "—" }),
        el("td", { text: cred.scope }),
        el("td", { class: "when", text: cred.last_used_at ? humanTime(cred.last_used_at) : "never" }),
        el("td", { class: "actions" }, [
          el("button", {
            class: "link danger",
            text: "Revoke",
            onclick: async () => {
              try {
                await api("DELETE", `/apppasswords/${cred.id}`);
                toast("Revoked");
                loadCredentials();
              } catch (err) { fail(err); }
            },
          }),
        ]),
      ]));
    }
    table.append(body);
    main.append(table);
  }

  async function loadCredentials() {
    try {
      const data = await api("GET", "/apppasswords/");
      state.entries = data.items || [];
      render();
    } catch (err) { fail(err); }
  }

  function createAppPassword() {
    openDialog({
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
      confirm: "Create",
      onSubmit: async (values) => {
        const created = await api("POST", "/apppasswords/", values);
        // The secret is shown exactly once; there is no way to recover it later.
        openDialog({
          title: "Copy this password now",
          body: "It cannot be shown again. Use your username with this value as the password.",
          fields: [],
          confirm: "Done",
          onSubmit: async () => { await loadCredentials(); },
          extra: el("div", { class: "mono", text: created.secret || "" }),
        });
      },
    });
  }

  // ---------- share landing page ----------

  async function renderShareLanding(token) {
    const root = $("#app");
    clear(root);

    let info;
    try {
      const resp = await fetch(`/s/${encodeURIComponent(token)}`);
      if (!resp.ok) throw new APIError(resp.status, "not_found", "This link is not available.");
      info = await resp.json();
    } catch {
      root.append(el("main", {}, [
        el("div", { class: "sharebox" }, [
          el("div", { class: "title", text: "Link not available" }),
          el("p", { class: "meta", text: "It may have expired, been revoked, or reached its usage limit." }),
        ]),
      ]));
      return;
    }

    const box = el("div", { class: "sharebox" });
    box.append(el("div", { class: "title", text: info.label || "Shared files" }));

    if (info.requires_password) {
      box.append(el("p", { class: "meta", text: "This link is protected. Enter the password to continue." }));
      const input = el("input", { type: "password", placeholder: "Password" });
      const submit = async () => {
        try {
          const resp = await fetch(`/s/${encodeURIComponent(token)}/verify`, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ password: input.value }),
          });
          if (!resp.ok) { toast("Wrong password", "error"); return; }
          renderShareLanding(token);
        } catch { toast("Wrong password", "error"); }
      };
      input.addEventListener("keydown", (e) => { if (e.key === "Enter") submit(); });
      box.append(el("label", {}, [el("span", { text: "Password" }), input]));
      box.append(el("button", { class: "primary", text: "Continue", onclick: submit }));
      root.append(el("main", {}, [box]));
      return;
    }

    if (info.kind === "request") {
      // A request share is a drop box: it never lists what is already inside.
      box.append(el("p", {
        class: "meta",
        text: info.max_uploads
          ? `Upload a file. ${Math.max(0, info.max_uploads - info.upload_count)} of ${info.max_uploads} slots left.`
          : "Upload a file to send it.",
      }));

      const input = el("input", { type: "file" });
      const button = el("button", { class: "primary", text: "Upload", onclick: async () => {
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
      }});

      box.append(el("label", {}, [el("span", { text: "File" }), input]));
      box.append(button);
      root.append(el("main", {}, [box]));
      return;
    }

    if (!info.can_read) {
      box.append(el("p", { class: "meta", text: "This link cannot be downloaded." }));
      root.append(el("main", {}, [box]));
      return;
    }

    const download = el("button", {
      class: "primary",
      text: "Download",
      onclick: () => { location.href = `/s/${encodeURIComponent(token)}/download`; },
    });
    box.append(download);

    if (info.children && info.children.length > 0) {
      const list = el("table", { class: "files" }, [
        el("tbody", {}, info.children.map((child) =>
          el("tr", {}, [
            el("td", { class: "name" }, [
              el("span", { class: "icon", text: child.kind === "dir" ? "📁" : "📄" }),
              el("span", { text: child.name }),
            ]),
            el("td", { class: "size", text: child.kind === "dir" ? "—" : humanSize(child.size) }),
            el("td", { class: "actions" }, child.kind === "dir" ? [] : [
              el("button", {
                class: "link",
                text: "Download",
                onclick: () => {
                  location.href = `/s/${encodeURIComponent(token)}/download?name=${encodeURIComponent(child.name)}`;
                },
              }),
            ]),
          ]),
        )),
      ]);
      box.append(el("div", { style: "margin-top:1.5rem" }, [list]));
    }

    root.append(el("main", {}, [box]));
  }

  // ---------- dialog ----------

  let activeDialog = null;

  function openDialog({ title, body, fields = [], confirm, danger, onSubmit, extra }) {
    if (activeDialog) activeDialog.close();

    const form = el("form", { method: "dialog" });
    form.append(el("h2", { text: title }));
    if (body) form.append(el("p", { class: "hint", text: body }));
    if (extra) form.append(extra);

    const values = {};
    for (const field of fields) {
      let input;
      if (field.type === "select") {
        input = el("select", { name: field.name });
        for (const opt of field.options) {
          input.append(el("option", { value: opt.value, text: opt.label }));
        }
      } else {
        input = el("input", {
          type: field.type,
          name: field.name,
          value: field.value ?? "",
          placeholder: field.placeholder ?? "",
          min: field.min ?? null,
          required: field.required ?? null,
        });
      }
      values[field.name] = input;
      form.append(el("label", {}, [el("span", { text: field.label }), input]));
    }

    const submitButton = el("button", {
      class: danger ? "danger" : "primary",
      text: confirm,
      type: "submit",
    });
    const cancelButton = el("button", { text: "Cancel", type: "button", onclick: () => dialog.close() });

    form.append(el("menu", {}, [cancelButton, submitButton]));

    const dialog = el("dialog", {}, [form]);
    document.body.append(dialog);
    activeDialog = dialog;

    dialog.addEventListener("close", () => { dialog.remove(); if (activeDialog === dialog) activeDialog = null; });

    form.addEventListener("submit", async (e) => {
      e.preventDefault();
      const out = {};
      for (const [name, input] of Object.entries(values)) out[name] = input.value.trim();

      submitButton.disabled = true;
      try {
        await onSubmit(out);
        dialog.close();
      } catch (err) {
        fail(err);
        submitButton.disabled = false;
      }
    });

    dialog.showModal();
    fields.length > 0 && values[fields[0].name]?.focus();
  }

  // ---------- routing ----------

  // switchTo changes the view and loads its data.
  //
  // Rendering without loading would show whatever the previous view left in
  // state.entries — the shares tab displaying file rows, and vice versa.
  function switchTo(view) {
    state.view = view;
    state.entries = [];
    render();
    loadView();
  }

  function loadView() {
    switch (state.view) {
      case "shares": return loadShares();
      case "trash": return loadTrash();
      case "credentials": return loadCredentials();
      default: return loadFiles();
    }
  }

  function render() {
    renderHeader();

    switch (state.view) {
      case "shares": renderShares(); break;
      case "trash": renderTrash(); break;
      case "credentials": renderCredentials(); break;
      default: renderFiles();
    }
  }

  async function boot() {
    // A /s/<token> URL is the anonymous share page and has no session.
    const shareMatch = location.pathname.match(/^\/s\/([^/]+)\/?$/);
    if (shareMatch) {
      state.shareToken = shareMatch[1];
      await renderShareLanding(state.shareToken);
      return;
    }

    try {
      state.me = await api("GET", "/auth/me");
    } catch (err) {
      if (err instanceof APIError && err.status === 401) {
        // Send them through the OIDC flow; it returns here afterwards.
        location.href = "/api/auth/login?next=" + encodeURIComponent(location.pathname);
        return;
      }
      throw err;
    }

    // Drop zone for uploads.
    const app = $("#app");
    if (app) {
      let depth = 0;
      app.addEventListener("dragenter", (e) => {
        e.preventDefault();
        depth++;
        app.id = "dropzone";
        app.classList.add("over");
      });
      app.addEventListener("dragover", (e) => e.preventDefault());
      app.addEventListener("dragleave", () => {
        if (--depth <= 0) { depth = 0; app.classList.remove("over"); }
      });
      app.addEventListener("drop", (e) => {
        e.preventDefault();
        depth = 0;
        app.classList.remove("over");
        if (state.view !== "files") return;
        if (e.dataTransfer?.files?.length) uploadFiles(e.dataTransfer.files);
      });
    }

    await loadView();
  }

  document.addEventListener("DOMContentLoaded", boot);
})();
