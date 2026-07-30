// Types mirroring the server's JSON.
//
// These are generated from openapi.yaml — see `pnpm gen:api` — and this file only gives the
// schema names shorter aliases. The document is the source of truth: writing the client's
// types by hand beside a hand-written client is how a client drifts from its server without
// anyone noticing, because the compiler keeps agreeing with the wrong shape.
//
// If a field here disagrees with the server, the fix is in openapi.yaml and then a
// regeneration, not an edit to this file. `pnpm gen:api:check` fails when the committed
// output is stale, so the two cannot quietly separate.

import type { components } from "./api-types";

type Schemas = components["schemas"];

export type ErrorBody = Schemas["Error"];
export type Me = Schemas["Me"];
export type FileNode = Schemas["FileNode"];
export type PreviewKind = Schemas["PreviewKind"];
export type NodeKind = FileNode["kind"];
export type Listing = Schemas["Listing"];
export type Version = Schemas["Version"];
export type VersionsResponse = Schemas["VersionsResponse"];
export type ShareKind = Schemas["ShareKind"];
export type Share = Schemas["Share"];
export type ShareCreated = Schemas["ShareCreated"];
export type AuditEvent = Schemas["AuditEvent"];
export type AuditPage = Schemas["AuditPage"];
export type AppPassword = Schemas["AppPassword"];
export type ShareLanding = Schemas["ShareLanding"];
