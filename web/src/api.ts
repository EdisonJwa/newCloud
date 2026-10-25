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
import type { paths, components } from "./api-types";
import type {
  AdminSetting,
  AppPassword,
  AuditPage,
  BatchRequest,
  BatchResponse,
  CreateShareRequest,
  FileNode,
  Group,
  Listing,
  Me,
  Share,
  ShareCreated,
  ShareLanding,
  SharedNode,
  VersionsResponse,
} from "./types";

/* --------------------------------------------------------------- social types --- */

/**
 * The social shapes come straight from the generated document, so a field the server adds shows up
 * here and a field it removes breaks the build rather than reading `undefined` at runtime.
 */
type Schemas = components["schemas"];
export type Tag = Schemas["Tag"];
export type Comment = Schemas["Comment"];
export type CommentThread = Schemas["CommentThread"];
export type EmptyTrashResult = Schemas["EmptyTrashResult"];
export type FavoriteToggle = Schemas["FavoriteToggle"];
export type SearchResults = Schemas["SearchResults"];
export type SearchResultNode = Schemas["FileNode"];

/** NodeRef names a node for the social endpoints: by path when the caller can spell one, by id
 *  otherwise. A file inside another account's namespace has no path in this caller's terms, so the
 *  id form is what makes starring a shared file possible. */
export interface NodeRef {
  id?: string;
  path?: string;
}

/** SearchParams is the §7.1 vocabulary. Every field is optional: an empty query with one filter is
 *  a listing, which is what the filter bar produces when the search box is cleared. */
export interface SearchParams {
  q?: string;
  kind?: "file" | "dir";
  mime?: string;
  size_min?: number;
  size_max?: number;
  modified_after?: string;
  modified_before?: string;
  tag?: string;
  favorite?: boolean;
  sort?: "modified" | "name" | "size";
  mode?: "name" | "content" | "all";
  cursor?: string;
  limit?: number;
}

/** A half-open byte interval, [start, end), as the upload endpoints report it. */
export interface UploadRange {
  start: number;
  end: number;
}

export interface UploadStatus {
  id: string;
  offset: number;
  chunk_size: number;
  ranges: UploadRange[];
}

export interface ChunkResult {
  offset: number;
  /** True on 409: the server already holds part of the range; `ranges` is authoritative. */
  conflict: boolean;
  ranges: UploadRange[];
}

export class APIError extends Error {
  readonly status: number;
  readonly code: string;
  /** Present on a 409 from the upload endpoints: the server's acknowledged ranges. */
  readonly ranges?: UploadRange[];

  constructor(status: number, code: string, message: string, ranges?: UploadRange[]) {
    super(message || code || `HTTP ${status}`);
    this.status = status;
    this.code = code;
    this.ranges = ranges;
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

/** callJson is the raw-fetch sibling of call(): same error shape, no generated wrapper. */
async function callJson<T>(url: string, init: RequestInit): Promise<T> {
  if (!online) {
    throw new APIError(0, "offline", "offline");
  }
  const resp = await fetch(url, { credentials: "same-origin", ...init });
  if (resp.status === 401) {
    throw new APIError(401, "unauthenticated", "unauthenticated");
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
    const body = (payload ?? {}) as {
      error?: string;
      message?: string;
      ranges?: UploadRange[];
    };
    throw new APIError(resp.status, body.error ?? "", body.message ?? "", body.ranges);
  }
  return payload as T;
}

export const api = {
  me: () => call<Me>(client.GET("/api/auth/me")),
  logout: () => call<void>(client.POST("/api/auth/logout")),

  list: (path: string) => call<Listing>(client.GET("/api/files/", { params: { query: { path } } })),
  recent: (limit = 12) =>
    call<{ items: FileNode[] }>(client.GET("/api/files/recent", { params: { query: { limit } } })),
  search: (params: SearchParams) =>
    call<SearchResults>(client.GET("/api/files/search", { params: { query: params as never } })),
  trash: () => call<{ items: FileNode[] }>(client.GET("/api/files/trash")),
  // Empty trash (core-scope §3).
  //
  // `more` is the whole client contract: the server purges in batches so a huge bin cannot time out,
  // and a client that sees it calls again. That is why this returns the result rather than a void —
  // "call me again" is an answer, not a failure to report.
  emptyTrash: () => call<EmptyTrashResult>(client.POST("/api/files/trash/empty")),

  /* ---------------------------------------------------------------- social --- */

  // Favorites (core-scope §4).
  //
  // Addressed by id OR path. Both are exposed because a file inside somebody else's namespace has no
  // path this caller can spell, so starring a shared file would otherwise be impossible. Idempotent
  // in both directions: a retried click is not an error.
  star: (node: NodeRef) => call<FavoriteToggle>(client.PUT("/api/files/favorite", { params: { query: node } })),
  unstar: (node: NodeRef) =>
    call<FavoriteToggle>(client.DELETE("/api/files/favorite", { params: { query: node } })),
  favorites: (limit = 50) =>
    call<{ items: FileNode[]; truncated: boolean; cursor?: string }>(
      client.GET("/api/favorites", { params: { query: { limit } } }),
    ),

  // Tags (core-scope §5).
  //
  // The listing carries `count` per tag, computed over nodes THIS caller can see — a count over every
  // node would tell them how many files they cannot reach carry a label.
  tags: () => call<{ items: Tag[] }>(client.GET("/api/tags/")),
  createTag: (body: { name: string; color?: string; system?: boolean }) =>
    call<Tag>(client.POST("/api/tags/", { body: body as never })),
  updateTag: (id: string, body: { name?: string; color?: string }) =>
    call<Tag>(client.PATCH("/api/tags/{id}", { params: { path: { id } }, body: body as never })),
  deleteTag: (id: string) => call<void>(client.DELETE("/api/tags/{id}", { params: { path: { id } } })),
  // Assignment is a metadata write, so it needs write on the node. The response is the node's
  // resulting tag set, which saves a refetch on every toggle.
  assignTag: (nodeID: string, tagID: string) =>
    call<{ id: string; tags: Tag[] }>(
      client.POST("/api/nodes/{id}/tags", {
        params: { path: { id: nodeID } },
        body: { tag_id: tagID } as never,
      }),
    ),
  unassignTag: (nodeID: string, tagID: string) =>
    call<{ id: string; tags: Tag[] }>(
      client.DELETE("/api/nodes/{id}/tags", {
        params: { path: { id: nodeID } },
        body: { tag_id: tagID } as never,
      }),
    ),

  // Comments (core-scope §6).
  //
  // `can_moderate` comes with the thread rather than being inferred from the file's permissions on
  // the client: the server is the only thing that knows, and a button that answers 404 is worse than
  // no button.
  comments: (nodeID: string, cursor?: string) =>
    call<CommentThread>(
      client.GET("/api/nodes/{id}/comments", {
        params: { path: { id: nodeID }, query: cursor ? { cursor } : {} },
      }),
    ),
  comment: (nodeID: string, body: string, parentID?: string) =>
    call<Comment>(
      client.POST("/api/nodes/{id}/comments", {
        params: { path: { id: nodeID } },
        body: { body, parent_id: parentID } as never,
      }),
    ),
  editComment: (id: string, body: string) =>
    call<Comment>(client.PATCH("/api/comments/{id}", {
      params: { path: { id } },
      body: { body } as never,
    })),
  deleteComment: (id: string) =>
    call<{ id: string; deleted: boolean }>(client.DELETE("/api/comments/{id}", { params: { path: { id } } })),

  versions: (path: string) =>
    call<VersionsResponse>(client.GET("/api/files/versions", { params: { query: { path } } })),
  // The server answers with the restored node (200), not 204: the caller can use it to refresh the
  // row it is showing, and the spec had said 204 — a contract the client and the server disagreed
  // about, found by the fourth independent review.
  restoreVersion: (path: string, version: number) =>
    call<FileNode>(client.POST("/api/files/restore-version", { body: { path, version } })),
  restore: (id: string) => call<void>(client.POST("/api/files/restore", { body: { id } })),
  mkdir: (path: string) => call<void>(client.POST("/api/files/mkdir", { body: { path } })),
  rename: (path: string, name: string) =>
    call<void>(client.POST("/api/files/rename", { body: { path, name } })),
  remove: (path: string) => call<void>(client.DELETE("/api/files/", { params: { query: { path } } })),

  // Copy and move, one item or many, through one endpoint.
  //
  // The response is per item because a multi-file transfer is routinely partial, and a single status
  // code cannot say which of five files moved. With exactly one item the server answers that item's
  // own status instead, so a single-file copy fails the way every other call fails — which is why
  // `target_exists` can arrive as an APIError or as a result row.
  batch: (body: BatchRequest) => call<BatchResponse>(client.POST("/api/files/batch", { body })),

  // Resumable uploads. Like the single-request upload, the chunk body is raw bytes, so these
  // three go through fetch rather than the generated client; the JSON in and out is still
  // shaped by the schema. The interesting case is 409: it carries the offset to resume from,
  // so it is returned rather than thrown.
  startUpload: (dir: string, name: string, size: number) =>
    callJson<UploadStatus>("/api/files/uploads", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ path: dir, name, size }),
    }),

  // uploadStatus is the authoritative resume state. The local pending record is only a hint; with
  // out-of-order chunks the highest accepted byte says nothing about which holes remain.
  uploadStatus: (id: string) =>
    callJson<UploadStatus>(`/api/files/uploads/${encodeURIComponent(id)}`, { method: "GET" }),

  putChunk: async (id: string, start: number, end: number, total: number, body: Blob): Promise<ChunkResult> => {
    const resp = await fetch(`/api/files/uploads/${encodeURIComponent(id)}`, {
      method: "PUT",
      credentials: "same-origin",
      headers: { "Content-Range": `bytes ${start}-${end}/${total}` },
      body,
    });
    const payload = (await resp.json().catch(() => ({}))) as {
      offset?: number;
      message?: string;
      ranges?: UploadRange[];
    };
    if (resp.status === 409) {
      // Not an error: the server is telling the client which bytes it already holds.
      return {
        offset: payload.offset ?? 0,
        conflict: true,
        ranges: payload.ranges ?? [],
      };
    }
    if (!resp.ok) {
      throw new APIError(resp.status, "chunk_failed", payload.message ?? "", payload.ranges);
    }
    return {
      offset: payload.offset ?? end + 1,
      conflict: false,
      ranges: payload.ranges ?? [{ start, end: end + 1 }],
    };
  },

  completeUpload: (id: string) =>
    callJson<FileNode>(`/api/files/uploads/${encodeURIComponent(id)}/complete`, { method: "POST" }),

  abortUpload: async (id: string) => {
    await fetch(`/api/files/uploads/${encodeURIComponent(id)}`, {
      method: "DELETE",
      credentials: "same-origin",
    });
  },

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
  // The body type is the schema, not a hand-written copy of it. It used to be a copy, and the copy
  // drifted from the handler: `kind: "email"` with `recipients` was accepted by the server and
  // unrepresentable here, so the create dialog could not offer an email share at all.
  createShare: (body: CreateShareRequest) => call<ShareCreated>(client.POST("/api/shares/", { body })),
  revokeShare: (id: string) =>
    call<void>(client.DELETE("/api/shares/{id}", { params: { path: { id } } })),

  // "Shared with me" — the other direction of sharing.
  //
  // These nodes are addressed by ID, not by path: their paths live in the owner's namespace, and
  // every path-based route resolves inside the caller's own tree. Without these two calls the ACL
  // API could create a grant that no browser could ever act on, which is exactly the state this
  // section exists to end. The listing carries the authorizer's CURRENT capability, so a revoked
  // grant disappears rather than going stale.
  sharedWithMe: () => call<{ items: SharedNode[] }>(client.GET("/api/shared")),
  sharedNode: (id: string) =>
    call<SharedNode>(client.GET("/api/shared/{id}", { params: { path: { id } } })),

  audit: (params: { offset?: number; actions?: string; result?: string; since?: string }) =>
    call<AuditPage>(client.GET("/api/audit/events", { params: { query: params } })),

  appPasswords: () => call<{ items: AppPassword[] }>(client.GET("/api/apppasswords/")),
  createAppPassword: (body: { label: string; scope: string }) =>
    call<{ secret: string; id: string; prefix: string }>(
      client.POST("/api/apppasswords/", { body: body as never }),
    ),
  revokeAppPassword: (id: string) =>
    call<void>(client.DELETE("/api/apppasswords/{id}", { params: { path: { id } } })),

  // Account administration. A non-admin gets 404 from these, so the UI hides the section rather
  // than showing an empty one — but the hiding is presentation; the server is the gate.
  adminUsers: (params: { status?: "active" | "pending" | "disabled"; limit?: number }) =>
    call(client.GET("/api/admin/users", { params: { query: params } })),
  setUserStatus: (id: string, status: "active" | "pending" | "disabled") =>
    call(client.POST("/api/admin/users/{id}/status", {
      params: { path: { id } },
      body: { status } as never,
    })),
  // Quotas are an administrative control (§4), so changing one is an administrative act with its
  // own audit event rather than a side effect of something else.
  setUserQuota: (id: string, bytes: number) =>
    call<{ id: string; quota: number; used_bytes: number }>(
      client.POST("/api/admin/users/{id}/quota", { params: { path: { id } }, body: { bytes } }),
    ),

  // Groups. Membership is what a group ACL resolves against, so an admin who cannot see or edit it
  // cannot use group sharing at all.
  adminGroups: () => call<{ groups: Group[] }>(client.GET("/api/admin/groups")),
  createGroup: (body: { name: string; display_name?: string }) =>
    call<Group>(client.POST("/api/admin/groups", { body })),
  addGroupMember: (id: string, body: { username?: string; user_id?: string; role?: string }) =>
    call<Group>(client.POST("/api/admin/groups/{id}/members", { params: { path: { id } }, body: body as never })),
  removeGroupMember: (id: string, userID: string) =>
    call<void>(client.DELETE("/api/admin/groups/{id}/members/{userID}", {
      params: { path: { id, userID } },
    })),

  // Runtime settings: the limits an administrator can change without a restart. Each row reports
  // its source so "the override is still on" is visible rather than inferred from a value that does
  // not match the file.
  adminSettings: () => call<{ settings: AdminSetting[] }>(client.GET("/api/admin/settings")),
  setAdminSetting: (key: string, value: number) =>
    call<{ key: string; value: number }>(
      client.PUT("/api/admin/settings/{key}", { params: { path: { key } }, body: value }),
    ),
  resetAdminSetting: (key: string) =>
    call<{ key: string; configured: number }>(
      client.DELETE("/api/admin/settings/{key}", { params: { path: { key } } }),
    ),

  // Resource ACL (§4.1 layer 2). Grants are per node, so every call names a path.
  //
  // The listing returns direct grants only: inherited ones are already reflected in what the
  // caller can do, and showing a whole ancestor chain would bury the two rows the owner set.
  acl: (path: string) => call(client.GET("/api/acl/", { params: { query: { path } } })),
  grant: (body: { path: string; subject: string; permissions: string[]; inherit?: boolean }) =>
    call(client.POST("/api/acl/", { body: body as never })),
  // The node path is required as well as the id: a grant must belong to the node named by
  // `path`. Ids are unguessable, but a mistake is not, so the server checks both.
  revokeGrant: (id: string, path: string) =>
    call(client.DELETE("/api/acl/{id}", { params: { path: { id }, query: { path } } })),
};

// Content URLs are plain links, not fetches: the browser handles range requests, caching and
// download disposition for us, and the server decides whether inline rendering is allowed.

export const downloadURL = (path: string): string => `/api/files/download?path=${encodeURIComponent(path)}`;

export const inlineURL = (path: string): string =>
  `/api/files/download?path=${encodeURIComponent(path)}&inline=1`;

export const thumbnailURL = (path: string): string =>
  `/api/files/thumbnail?path=${encodeURIComponent(path)}`;

// A ZIP of the selection. A plain link rather than a fetch: the browser handles the download, the
// response can be gigabytes, and the server streams it, so there is nothing to buffer here.
export const zipURL = (paths: string[]): string => {
  const query = new URLSearchParams();
  for (const p of paths) query.append("path", p);
  return `/api/files/zip?${query.toString()}`;
};

// A node somebody else owns, addressed by id.
//
// The `download` permission is enforced per request; a plain link is the right shape because the
// browser handles the response, and the route is authenticated, so the session cookie does the work.
export const sharedContentURL = (id: string): string =>
  `/api/shared/${encodeURIComponent(id)}/content`;

// A share's download URL.
//
// It points at the share route, not at /api/files/download. The landing page is anonymous — it has
// no session and, once the content origin is split out, no cookie either — so an authenticated API
// route is the wrong address both for authorization and for origin: the bytes belong to the content
// vhost (NC-RR-006), which is the one the share download route lives on.
//
// `name` selects a child of a shared directory; omitting it downloads the shared file itself.
// Putting the path in `label` (the old behaviour) was wrong: label is a human-readable title the
// owner typed, not a filesystem path.
export const shareDownloadURL = (token: string, name?: string): string => {
  const base = `/s/${encodeURIComponent(token)}/download`;
  return name ? `${base}?name=${encodeURIComponent(name)}` : base;
};

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
