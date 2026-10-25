// Shared interpretations of a node.
//
// Three surfaces now display a file or folder — Files, Home and Sharing — and two of them
// receive nodes from endpoints that send a different subset of fields than `/api/files/` does.
// A received node carries a MIME type but no `preview`; an audit event carries a name in
// `metadata`; a share carries a path but no node. Each of those needs the same small mapping,
// and having three copies of it is how "Documents" drifts from "Document" between screens.
//
// Nothing here is a component. It is the vocabulary: what a permission string means to a
// recipient, what a MIME type is previewable as, and how a path is abbreviated for a subtitle.

import { extensionOf } from "./ui";
import type { FileNode } from "./types";

/**
 * previewOf infers a preview kind from a MIME type and a name.
 *
 * The server sends `preview` on its own listings; a received node's payload carries only the
 * MIME type. Rather than a second endpoint, this mirrors the server's mapping. An unrecognised
 * type returns `undefined`, which the viewer treats as "no inline render" rather than guessing
 * an image and showing a broken one.
 */
export function previewOf(mime: string | undefined, name: string): FileNode["preview"] {
  const type = (mime ?? "").toLowerCase();
  const ext = extensionOf(name).toLowerCase();
  if (type.startsWith("image/")) return "image";
  if (type.startsWith("video/")) return "video";
  if (type.startsWith("audio/")) return "audio";
  if (type === "application/pdf") return "pdf";
  if (type.startsWith("text/") || ["md", "txt", "log", "json", "yaml", "yml", "csv", "tsv"].includes(ext)) {
    return "text";
  }
  return undefined;
}

/**
 * accessLabel turns the server's resolved permission list into the two words a recipient
 * needs.
 *
 * The raw string (`read+list+download`) is shown in the Inspector, where somebody is asking
 * exactly that question. A row says Can view or Can edit, which is what the choice was about.
 * `write`/`delete`/`upload` are the primitives that make an item editable; `read`, `list` and
 * `download` do not.
 */
export function accessLabel(t: (key: string) => string, permissions: string): string {
  const perms = (permissions ?? "").toLowerCase();
  if (perms.includes("write") || perms.includes("delete") || perms.includes("upload")) {
    return t("access.canEdit");
  }
  return t("access.canView");
}

/** parentOf is the directory part of a workspace-relative path, or "" at the root. */
export function parentOf(path: string): string {
  const clean = path.replace(/^\//, "");
  const i = clean.lastIndexOf("/");
  return i < 0 ? "" : clean.slice(0, i);
}

/** leafOf is the last segment of a path, or the path itself when it has none. */
export function leafOf(path: string): string {
  return path.slice(path.lastIndexOf("/") + 1) || path;
}

/**
 * auditTarget picks the most specific human identifier an audit event has.
 *
 * The audit payload does not always carry a name: an older writer recorded only a target id.
 * Showing the id is honest about that rather than inventing a filename.
 */
export function auditTarget(
  event: { metadata?: Record<string, unknown>; target_id?: string },
  t: (key: string) => string,
): string {
  const meta = event.metadata ?? {};
  const path = typeof meta.path === "string" ? meta.path : "";
  const name = typeof meta.name === "string" ? meta.name : "";
  if (path) return path;
  if (name) return name;
  if (event.target_id) return event.target_id.slice(0, 8);
  return t("activity.system");
}

/**
 * sharedContentId reports whether a `path` field on a node is really an id.
 *
 * Received nodes are addressed by id because their paths live in the owner's namespace. The
 * viewer stores that id in `path` so that one code path can carry it; this is the test that
 * tells download and thumbnail URLs which route to use. A UUID has dashes and no slash; a real
 * path never looks like one.
 */
export function looksLikeNodeId(value: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value);
}
