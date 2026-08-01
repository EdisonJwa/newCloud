// API client.
//
// The calls are typed and generated from openapi.yaml: `client.GET("/api/files/", ...)` only
// compiles if the path and its parameters exist in the document, and the response type comes
// from the schema rather than from a hand-written interface that could drift. Adding an
// endpoint means changing the document and regenerating, not editing this file.
//
// Two things are deliberately not routed through it:
//
//   * uploads, whose body is a stream and which need an AbortSignal, and
//   * the public share-landing calls, which are unauthenticated, have no session, and answer
//     both HTML and JSON on the same URL.
//
// Both are marked below. Everything else is one line per endpoint, which is the point.

import createClient from "openapi-fetch";
import type { paths } from "./api-types";
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

const client = createClient<paths>({
  // Same origin. An empty base keeps the generated paths as they appear in the document.
  baseUrl: "",
  credentials: "same-origin",
  headers: { Accept: "application/json" },
});

/**
 * call unwraps an openapi-fetch result into a value or an APIError.
 *
 * Every failure the UI shows comes through here, so the error shape is decided once: the
 * server's own code and message, never a client-invented summary of them.
 */
async function call<T>(request: Promise<{ data?: T; error?: unknown; response: Response }>): Promise<T> {
  if (!online) {
    throw new APIError(0, "offline", "offline");
  }

  const { data, error, response } = await request;

  if (response.status === 401) {
    throw new APIError(401, "unauthenticated", "unauthenticated");
  }
  if (!response.ok) {
    const body = (error ?? {}) as { error?: string; message?: string };
    throw new APIError(response.status, body.error ?? "", body.message ?? "");
  }
  return data as T;
}

export const api = {
  me: () => call<Me>(client.GET("/api/auth/me")),
  logout: () => call<void>(client.POST("/api/auth/logout")),

  list: (path: string) => call<Listing>(client.GET("/api/files/", { params: { query: { path } } })),
  recent: (limit = 12) =>
    call<{ items: FileNode[] }>(client.GET("/api/files/recent", { params: { query: { limit } } })),
  search: (q: string, limit = 12) =>
    call<{ items: FileNode[]; query: string }>(
      client.GET("/api/files/search", { params: { query: { q, limit } } }),
    ),
  trash: () => call<{ items: FileNode[] }>(client.GET("/api/files/trash")),
  versions: (path: string) =>
    call<VersionsResponse>(client.GET("/api/files/versions", { params: { query: { path } } })),
  restoreVersion: (path: string, version: number) =>
    call<void>(client.POST("/api/files/restore-version", { body: { path, version } })),
  restore: (id: string) => call<void>(client.POST("/api/files/restore", { body: { id } })),
  mkdir: (path: string) => call<void>(client.POST("/api/files/mkdir", { body: { path } })),
  rename: (path: string, name: string) =>
    call<void>(client.POST("/api/files/rename", { body: { path, name } })),
  remove: (path: string) => call<void>(client.DELETE("/api/files/", { params: { query: { path } } })),

  // Not generated: the body is a stream and the caller needs to cancel it. Failures are turned
  // into the same APIError shape so callers cannot tell the difference.
  upload: (dir: string, file: File, signal?: AbortSignal) => {
    if (!online) {
      return Promise.reject(new APIError(0, "offline", "offline"));
    }
    const form = new FormData();
    form.append("path", dir);
    form.append("file", file);
    return fetch("/api/files/upload", {
      method: "POST",
      body: form,
      credentials: "same-origin",
      signal,
    }).then(async (resp) => {
      if (!resp.ok) {
        const text = await resp.text();
        let payload: { error?: string; message?: string } = {};
        try {
          payload = JSON.parse(text);
        } catch {
          payload = { message: text };
        }
        throw new APIError(resp.status, payload.error ?? "", payload.message ?? "");
      }
      return (await resp.json()) as FileNode;
    });
  },

  shares: () => call<{ items: Share[] }>(client.GET("/api/shares/")),
  createShare: (body: {
    path: string;
    kind: string;
    password?: string;
    expires_in_seconds?: number;
    max_downloads?: number;
    max_uploads?: number;
  }) => call<ShareCreated>(client.POST("/api/shares/", { body: body as never })),
  revokeShare: (id: string) =>
    call<void>(client.DELETE("/api/shares/{id}", { params: { path: { id } } })),

  audit: (params: { offset?: number; actions?: string; result?: string; since?: string }) =>
    call<AuditPage>(client.GET("/api/audit/events", { params: { query: params } })),

  appPasswords: () => call<{ items: AppPassword[] }>(client.GET("/api/apppasswords/")),
  createAppPassword: (body: { label: string; scope: string }) =>
    call<{ secret: string; id: string; prefix: string }>(
      client.POST("/api/apppasswords/", { body: body as never }),
    ),
  revokeAppPassword: (id: string) =>
    call<void>(client.DELETE("/api/apppasswords/{id}", { params: { path: { id } } })),
};

// Content URLs are plain links, not fetches: the browser handles range requests, caching and
// download disposition for us, and the server decides whether inline rendering is allowed.

export const downloadURL = (path: string): string => `/api/files/download?path=${encodeURIComponent(path)}`;

export const inlineURL = (path: string): string =>
  `/api/files/download?path=${encodeURIComponent(path)}&inline=1`;

export const thumbnailURL = (path: string): string =>
  `/api/files/thumbnail?path=${encodeURIComponent(path)}`;

// The public share landing speaks a different, unauthenticated protocol: no session, and the
// same URL answers HTML to a navigation and JSON to a fetch. None of that belongs in the
// generated client, which assumes a signed-in JSON API.

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
