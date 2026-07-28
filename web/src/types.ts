// Types mirroring the server's JSON.
//
// These are hand-written rather than generated from an OpenAPI document. The trade-off is
// recorded in docs/0006: with fifteen endpoints and one consumer, a generator plus its schema
// maintenance costs more than it saves. That changes the moment a second consumer appears
// (a CLI, a mobile client) or the surface grows substantially — at that point an
// `openapi.yaml` becomes the single source of truth and these are generated from it.

export type NodeKind = "file" | "dir";

/** How the server is willing to render a file. Absent means "download only". */
export type PreviewKind = "image" | "video" | "audio" | "pdf" | "text";

export interface FileNode {
  id: string;
  name: string;
  path: string;
  kind: NodeKind;
  size: number;
  mime?: string;
  etag?: string;
  modified_at: string;
  created_at: string;
  trashed?: boolean;
  /** Advisory: the download endpoint re-checks the type and the bytes. */
  preview?: PreviewKind;
  /** Number of active links pointing at this file. Absent when zero. */
  share_count?: number;
  /** Whether earlier versions are stored. */
  has_versions?: boolean;
}

export interface Listing {
  path: string;
  name: string;
  children: FileNode[];
  truncated: boolean;
}

export interface Version {
  version: number;
  size: number;
  created_at: string;
}

export interface VersionsResponse {
  path: string;
  versions: Version[];
  cheap_versions: boolean;
}

export type ShareKind = "link" | "email" | "request";

export interface Share {
  id: string;
  node_id: string;
  kind: ShareKind;
  label: string;
  url?: string;
  node_path: string;
  has_password: boolean;
  expires_at?: string;
  max_downloads?: number;
  download_count: number;
  max_uploads?: number;
  upload_count: number;
  permissions: string[];
  hide_contents: boolean;
  recipients?: string[];
  created_at: string;
  revoked_at?: string;
}

export interface ShareCreated {
  share: Share;
  token: string;
  note: string;
}

export interface AuditEvent {
  id: number;
  ts: string;
  actor_kind: string;
  actor_user_id?: string;
  actor_email?: string;
  actor_ip?: string;
  user_agent?: string;
  action: string;
  target_kind: string;
  target_id?: string;
  share_id?: string;
  result: string;
  bytes?: number;
  metadata?: Record<string, unknown>;
}

export interface AuditPage {
  events: AuditEvent[];
  total: number;
  offset: number;
  page_size: number;
  has_more: boolean;
  actions: string[];
}

export interface AppPassword {
  id: string;
  label: string;
  prefix: string;
  scope: string;
  created_at: string;
  expires_at?: string;
  last_used?: string;
  revoked: boolean;
}

export interface Me {
  id: string;
  username: string;
  email: string;
  display_name?: string;
  role: string;
  quota_bytes?: number;
  used_bytes?: number;
  storage?: { layout?: string; backend?: string };
}

export interface ShareLanding {
  kind: ShareKind;
  label?: string;
  can_read: boolean;
  requires_password: boolean;
  requires_email: boolean;
  expires_at?: string;
  max_uploads?: number;
  upload_count: number;
  children?: FileNode[];
}
