// API client.
//
// Everything goes through `request`, so there is one place that handles the session, offline
// state, and error shape. A failed call raises APIError with the server's code and message,
// which the UI shows verbatim rather than inventing its own wording for a server condition.

import type {
  AppPassword,
  AuditPage,
  FileNode,
  Listing,
  Me,
  Share,
  ShareCreated,
  ShareLanding,
  VersionsResponse,
} from "./types";

export class APIError extends Error {
  readonly status: number;
  readonly code: string;

  constructor(status: number, code: string, message: string) {
    super(message || code || `HTTP ${status}`);
    this.status = status;
    this.code = code;
  }

  /** A denial is the same shape as a miss: the server never reveals whether an object exists. */
  get isNotFound(): boolean {
    return this.status === 404 || this.status === 403;
  }
}

let online = true;

export function setOnline(value: boolean): void {
  online = value;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  if (!online) {
    throw new APIError(0, "offline", "offline");
  }

  const init: RequestInit = { method, credentials: "same-origin", headers: {} };
  if (body instanceof FormData) {
    init.body = body;
  } else if (body !== undefined) {
    init.headers = { "Content-Type": "application/json" };
    init.body = JSON.stringify(body);
  }

  const resp = await fetch("/api" + path, init);

  if (resp.status === 401) {
    throw new APIError(401, "unauthenticated", "unauthenticated");
  }
  if (resp.status === 204) {
    return undefined as T;
  }

  const text = await resp.text();
  let payload: unknown = null;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      payload = { message: text };
    }
  }

  if (!resp.ok) {
    const p = (payload ?? {}) as { error?: string; message?: string };
    throw new APIError(resp.status, p.error ?? "", p.message ?? "");
  }
  return payload as T;
}

const q = (params: Record<string, string | number | undefined>): string => {
  const search = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== "") search.set(k, String(v));
  }
  const s = search.toString();
  return s ? `?${s}` : "";
};

export const api = {
  me: () => request<Me>("GET", "/auth/me"),
  logout: () => request<void>("POST", "/auth/logout"),

  list: (path: string) => request<Listing>("GET", `/files/${q({ path })}`),
  trash: () => request<{ items: FileNode[] }>("GET", "/files/trash"),
  recent: (limit = 12) => request<{ items: FileNode[] }>("GET", `/files/recent?limit=${limit}`),
  versions: (path: string) => request<VersionsResponse>("GET", `/files/versions${q({ path })}`),
  restoreVersion: (path: string, version: number) =>
    request<void>("POST", "/files/restore-version", { path, version }),
  restore: (id: string) => request<void>("POST", "/files/restore", { id }),
  mkdir: (path: string) => request<void>("POST", "/files/mkdir", { path }),
  rename: (path: string, name: string) => request<void>("POST", "/files/rename", { path, name }),
  remove: (path: string) => request<void>("DELETE", `/files/${q({ path })}`),

  upload: (dir: string, file: File, signal?: AbortSignal) => {
    const form = new FormData();
    form.append("path", dir);
    form.append("file", file);
    // `request` does not take a signal; uploads are the one call that benefits from one, so
    // the fetch is issued directly here and shares the same error shape.
    return fetch("/api/files/upload", {
      method: "POST",
      body: form,
      credentials: "same-origin",
      signal,
    }).then(async (resp) => {
      if (!resp.ok) {
        const text = await resp.text();
        let p: { error?: string; message?: string } = {};
        try {
          p = JSON.parse(text);
        } catch {
          p = { message: text };
        }
        throw new APIError(resp.status, p.error ?? "", p.message ?? "");
      }
      return (await resp.json()) as FileNode;
    });
  },

  shares: () => request<{ items: Share[] }>("GET", "/shares/"),
  createShare: (body: {
    path: string;
    kind: string;
    password?: string;
    expires_in_seconds?: number;
    max_downloads?: number;
    max_uploads?: number;
  }) => request<ShareCreated>("POST", "/shares/", body),
  revokeShare: (id: string) => request<void>("DELETE", `/shares/${id}`),

  audit: (params: { offset?: number; actions?: string; result?: string; since?: string }) =>
    request<AuditPage>("GET", `/audit/events${q(params)}`),

  appPasswords: () => request<{ items: AppPassword[] }>("GET", "/apppasswords/"),
  createAppPassword: (body: { label: string; scope: string }) =>
    request<{ secret: string; id: string; prefix: string }>("POST", "/apppasswords/", body),
  revokeAppPassword: (id: string) => request<void>("DELETE", `/apppasswords/${id}`),
};

// Content URLs are plain links, not fetches: the browser handles range requests, caching and
// download disposition for us, and the server decides whether inline rendering is allowed.

export const downloadURL = (path: string): string =>
  `/api/files/download${q({ path })}`;

export const inlineURL = (path: string): string =>
  `/api/files/download${q({ path, inline: "1" })}`;

export const thumbnailURL = (path: string): string =>
  `/api/files/thumbnail${q({ path })}`;

// The public share landing page speaks a different, unauthenticated protocol.

export async function fetchShareLanding(token: string): Promise<ShareLanding> {
  const resp = await fetch(`/s/${encodeURIComponent(token)}`, {
    headers: { Accept: "application/json" },
  });
  if (!resp.ok) throw new APIError(resp.status, "unavailable", "");
  return (await resp.json()) as ShareLanding;
}

export async function verifySharePassword(token: string, password: string): Promise<boolean> {
  const resp = await fetch(`/s/${encodeURIComponent(token)}/verify`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ password }),
  });
  return resp.ok;
}

export async function requestShareCode(token: string, email: string): Promise<void> {
  // Always 202: revealing whether an address is invited would turn this into an oracle.
  await fetch(`/s/${encodeURIComponent(token)}/recipient`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email }),
  });
}

export async function verifyShareCode(token: string, email: string, code: string): Promise<boolean> {
  const resp = await fetch(`/s/${encodeURIComponent(token)}/recipient/verify`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, code }),
  });
  return resp.ok;
}

export async function uploadToShare(token: string, file: File): Promise<string> {
  const form = new FormData();
  form.append("file", file);
  const resp = await fetch(`/s/${encodeURIComponent(token)}/upload`, {
    method: "POST",
    body: form,
  });
  const payload = (await resp.json().catch(() => ({}))) as { name?: string; message?: string };
  if (!resp.ok) throw new APIError(resp.status, "upload_failed", payload.message ?? "");
  return payload.name ?? file.name;
}
