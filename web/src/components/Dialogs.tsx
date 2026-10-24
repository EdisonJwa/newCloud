// Dialogs.
//
// Native <dialog> with showModal(): the platform provides the top layer, focus containment,
// Escape handling and the inert backdrop, none of which is worth reimplementing. What is
// added here is the form plumbing and the one thing the platform does not do — returning
// focus to the element that opened the dialog.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { useI18n } from "../i18n";
import { Icon, useToast } from "../ui";
import { api, APIError } from "../api";
import type { CreateShareRequest, FileNode } from "../types";

interface ModalProps {
  title: string;
  description?: string;
  children?: ReactNode;
  footer: ReactNode;
  onClose: () => void;
}

function Modal({ title, description, children, footer, onClose }: ModalProps) {
  const ref = useRef<HTMLDialogElement>(null);
  const opener = useRef<Element | null>(null);

  useEffect(() => {
    opener.current = document.activeElement;
    ref.current?.showModal();
    return () => {
      // Returning focus matters: without it the next Tab starts from the top of the page and
      // a keyboard user loses their place after every dialog.
      if (opener.current instanceof HTMLElement) opener.current.focus();
    };
  }, []);

  return (
    <dialog
      ref={ref}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <div className="head">
        <h2>{title}</h2>
        {description ? <p>{description}</p> : null}
      </div>
      {children ? <div className="body">{children}</div> : null}
      <div className="foot">{footer}</div>
    </dialog>
  );
}

function errText(err: unknown): string {
  return err instanceof APIError ? err.message : String(err);
}

export function NewFolderDialog({
  dir,
  onClose,
  onDone,
}: {
  dir: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const toast = useToast();
  const [name, setName] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    const value = name.trim();
    if (!value) return;
    setBusy(true);
    try {
      await api.mkdir(dir ? `${dir}/${value}` : value);
      toast.info(t("toast.folderCreated"));
      onDone();
      onClose();
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  return (
    <Modal
      title={t("dlg.newFolder")}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="btn primary" disabled={busy || !name.trim()} onClick={submit}>
            {t("dlg.create")}
          </button>
        </>
      }
    >
      <label className="field">
        <span className="micro">{t("dlg.folderLabel")}</span>
        <input
          value={name}
          autoFocus
          aria-label={t("dlg.folderLabel")}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void submit();
          }}
        />
      </label>
    </Modal>
  );
}

export function RenameDialog({
  entry,
  onClose,
  onDone,
}: {
  entry: FileNode;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const toast = useToast();
  const [name, setName] = useState(entry.name);
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    const next = name.trim();
    if (!next || next === entry.name) {
      onClose();
      return;
    }
    setBusy(true);
    try {
      await api.rename(entry.path, next);
      toast.info(t("toast.renamed"));
      onDone();
      onClose();
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  return (
    <Modal
      title={t("dlg.rename")}
      description={entry.path}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="btn primary" disabled={busy} onClick={submit}>
            {t("dlg.rename")}
          </button>
        </>
      }
    >
      <label className="field">
        <span className="micro">{t("dlg.renameLabel")}</span>
        <input
          value={name}
          autoFocus
          aria-label={t("dlg.renameLabel")}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") void submit();
          }}
        />
      </label>
    </Modal>
  );
}

export function DeleteDialog({
  entries,
  onClose,
  onDone,
}: {
  entries: FileNode[];
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const toast = useToast();
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    setBusy(true);
    const failures: string[] = [];
    for (const entry of entries) {
      try {
        await api.remove(entry.path);
      } catch (err) {
        failures.push(`${entry.name}: ${errText(err)}`);
      }
    }
    onDone();
    onClose();
    if (failures.length === 0) {
      toast.info(t("toast.deleted", { n: entries.length }));
    } else {
      // Partial failure is reported rather than swallowed: the user has to know which items
      // are still there.
      toast.error(failures[0] ?? "");
    }
  };

  return (
    <Modal
      title={
        entries.length === 1
          ? t("dlg.deleteOne", { name: entries[0]?.name ?? "" })
          : t("dlg.deleteMany", { n: entries.length })
      }
      description={t("dlg.deleteBody")}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="btn primary" disabled={busy} onClick={submit}>
            {t("detail.delete")}
          </button>
        </>
      }
    />
  );
}

/** The three shapes a link can take. `request` is only offered for a directory. */
type ShareMode = "link" | "email" | "request";

/** parseRecipients turns a pasted list into addresses.
 *
 *  People paste addresses separated by commas, semicolons, spaces or newlines, usually with a
 *  trailing separator. Accepting all of them is friendlier than demanding one, and it costs
 *  nothing: the server re-validates every address with net/mail and is the authority. Addresses
 *  are lower-cased and de-duplicated because the stored form is lower-cased — sending both cases
 *  would create two recipient rows for one person.
 */
function parseRecipients(raw: string): string[] {
  const seen = new Set<string>();
  for (const part of raw.split(/[,;\s]+/)) {
    const addr = part.trim().replace(/^<|>$/g, "").toLowerCase();
    if (addr) seen.add(addr);
  }
  return [...seen];
}

/** looksLikeEmail is the cheap shape check, deliberately no stricter than the server's.
 *
 *  internal/share accepts `user@host` without a dot (an intranet address is still an address). A
 *  client rule that demanded a dot would refuse something the server would happily deliver to.
 */
const looksLikeEmail = (addr: string): boolean => /^[^@\s]+@[^@\s]+$/.test(addr);

export function ShareDialog({ entry, onClose }: { entry: FileNode; onClose: () => void }) {
  const { t } = useI18n();
  const toast = useToast();
  const [kind, setKind] = useState<ShareMode>("link");
  const [password, setPassword] = useState("");
  const [hours, setHours] = useState("");
  const [maxUses, setMaxUses] = useState("");
  const [recipients, setRecipients] = useState("");
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<{ url: string; recipients: string[] } | null>(null);

  const addresses = parseRecipients(recipients);
  const invalid = addresses.filter((a) => !looksLikeEmail(a));
  // An email share with no recipients is refused by the server and would be a link that opens for
  // nobody: the kind alone is what makes the recipient check apply, and it has nothing to match.
  const missingRecipients = kind === "email" && addresses.length === 0;
  const canSubmit = !busy && !missingRecipients && invalid.length === 0;

  const submit = async () => {
    setBusy(true);
    try {
      const body: CreateShareRequest = { path: entry.path, kind };
      if (password) body.password = password;
      const h = Number.parseInt(hours, 10);
      if (h > 0) body.expires_in_seconds = h * 3600;
      const m = Number.parseInt(maxUses, 10);
      if (m > 0) {
        if (kind === "request") body.max_uploads = m;
        else body.max_downloads = m;
      }
      if (kind === "email") body.recipients = addresses;

      const res = await api.createShare(body);
      const url = `${location.origin}/s/${res.token}`;
      await navigator.clipboard.writeText(url).catch(() => {});
      setCreated({ url, recipients: res.share?.recipients ?? [] });
      setBusy(false);
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  if (created) {
    return (
      <Modal
        title={t("dlg.linkReady")}
        description={t("dlg.linkReadyBody")}
        onClose={onClose}
        footer={
          <button type="button" className="btn primary" onClick={onClose}>
            {t("dlg.done")}
          </button>
        }
      >
        <div className="copybox">{created.url}</div>
        {created.recipients.length > 0 ? (
          <p className="hintline">
            {t("dlg.emailInvited", { list: created.recipients.join(", ") })}
          </p>
        ) : null}
      </Modal>
    );
  }

  return (
    <Modal
      title={t("dlg.shareTitle", { name: entry.name })}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="btn primary" disabled={!canSubmit} onClick={submit}>
            {t("dlg.createLink")}
          </button>
        </>
      }
    >
      <label className="field">
        <span className="micro">{t("dlg.shareKind")}</span>
        <select
          value={kind}
          onChange={(e) => setKind(e.target.value as ShareMode)}
        >
          <option value="link">{t("dlg.shareLink")}</option>
          <option value="email">{t("dlg.shareEmail")}</option>
          {entry.kind === "dir" ? <option value="request">{t("dlg.shareRequest")}</option> : null}
        </select>
      </label>

      {kind === "email" ? (
        <label className="field">
          <span className="micro">{t("dlg.recipients")}</span>
          <textarea
            value={recipients}
            rows={3}
            autoFocus
            placeholder="name@example.com, other@example.org"
            aria-describedby="recipients-hint"
            onChange={(e) => setRecipients(e.target.value)}
          />
          <span className="hintline" id="recipients-hint">
            {t("dlg.recipientsHint")}
          </span>
          {missingRecipients ? <span className="hintline">{t("dlg.recipientsRequired")}</span> : null}
          {invalid.length > 0 ? (
            <span className="hintline">{t("dlg.recipientsInvalid", { list: invalid.join(", ") })}</span>
          ) : null}
        </label>
      ) : null}

      <label className="field">
        <span className="micro">{t("dlg.password")}</span>
        <input type="password" value={password} onChange={(e) => setPassword(e.target.value)} />
      </label>
      <label className="field">
        <span className="micro">{t("dlg.expires")}</span>
        <input type="number" min="0" value={hours} onChange={(e) => setHours(e.target.value)} />
      </label>
      <label className="field">
        <span className="micro">{t("dlg.maxUses")}</span>
        <input type="number" min="0" value={maxUses} onChange={(e) => setMaxUses(e.target.value)} />
      </label>
    </Modal>
  );
}

export function NewPasswordDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const { t } = useI18n();
  const toast = useToast();
  const [label, setLabel] = useState("");
  const [scope, setScope] = useState("rw");
  const [busy, setBusy] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);

  const submit = async () => {
    setBusy(true);
    try {
      const res = await api.createAppPassword({ label, scope });
      setSecret(res.secret);
      setBusy(false);
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  if (secret) {
    return (
      <Modal
        title={t("access.secretTitle")}
        description={t("access.secretBody")}
        onClose={() => {
          onDone();
          onClose();
        }}
        footer={
          <button
            type="button"
            className="btn primary"
            onClick={() => {
              onDone();
              onClose();
            }}
          >
            {t("dlg.done")}
          </button>
        }
      >
        <div className="copybox">{secret}</div>
      </Modal>
    );
  }

  return (
    <Modal
      title={t("access.newPassword")}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="btn primary" disabled={busy} onClick={submit}>
            {t("dlg.create")}
          </button>
        </>
      }
    >
      <label className="field">
        <span className="micro">{t("access.label")}</span>
        <input value={label} autoFocus placeholder="laptop, rclone" onChange={(e) => setLabel(e.target.value)} />
      </label>
      <label className="field">
        <span className="micro">{t("access.scope")}</span>
        <select value={scope} onChange={(e) => setScope(e.target.value)}>
          <option value="rw">{t("access.scopeRW")}</option>
          <option value="ro">{t("access.scopeRO")}</option>
        </select>
      </label>
    </Modal>
  );
}
/* ---------- copy / move ---------- */

export type TransferOp = "copy" | "move";

/** Chunk size for one batch request.
 *
 *  Not a server limit ? the endpoint accepts up to 200 items ? but the granularity at which
 *  progress becomes visible and Stop becomes meaningful. Twenty-five is roughly a screenful.
 */
const TRANSFER_CHUNK = 25;

/** TransferDialog copies or moves a selection into a folder the person picks.
 *
 *  Three steps in one dialog because they are one task: choose the destination, watch it happen
 *  (and be able to stop it), and answer the conflict question if one comes up. Three separate
 *  dialogs would turn a two-second operation into a wizard.
 *
 *  Progress and Stop are real rather than decorative: the items go in chunks, so the counter
 *  advances as each batch completes and Stop ends the run between them. What already succeeded
 *  stays ? the alternative is a rollback of a partial operation, which this API deliberately does
 *  not promise.
 */
export function TransferDialog({
  op,
  entries,
  startDir,
  onClose,
  onDone,
}: {
  op: TransferOp;
  entries: FileNode[];
  startDir: string;
  onClose: () => void;
  onDone: () => void;
}) {
  const { t } = useI18n();
  const toast = useToast();

  const [step, setStep] = useState<"pick" | "run" | "conflict">("pick");
  const [dir, setDir] = useState(startDir);
  const [folders, setFolders] = useState<FileNode[] | null>(null);
  const [listError, setListError] = useState<string | null>(null);
  const [progress, setProgress] = useState({ done: 0, total: entries.length });
  const [conflicts, setConflicts] = useState<FileNode[]>([]);

  // Totals and the stop flag live in refs, not state: the retry after a conflict is a second run of
  // the same transfer, and resetting plain state there would make the final message report only the
  // retried items. The stop flag must also be readable inside the await loop, where state is stale.
  const stopped = useRef(false);
  const okTotal = useRef(0);
  const failTotal = useRef<{ name: string; code: string }[]>([]);
  // Items whose outcome the client genuinely does not know. See the catch in run(): a request that
  // failed as a whole may have been applied in part, and the response cannot say which items made it.
  const unknownTotal = useRef<{ name: string }[]>([]);

  // The destination browser lists directories only: a transfer target is a folder, and showing files
  // would invite the reading "put it inside this file".
  useEffect(() => {
    if (step !== "pick") return;
    let live = true;
    setFolders(null);
    setListError(null);
    api
      .list(dir)
      .then((res) => {
        if (live) setFolders((res.children ?? []).filter((e) => e.kind === "dir"));
      })
      .catch((err) => {
        if (live) setListError(err instanceof APIError ? err.message || err.code : String(err));
      });
    return () => {
      live = false;
    };
  }, [dir, step]);

  const targetFor = (entry: FileNode): string => (dir ? `${dir}/${entry.name}` : entry.name);

  /** finish reports the outcome and closes. */
  const finish = () => {
    onDone();
    const failed = failTotal.current;
    const unknown = unknownTotal.current;

    // Unknown comes first, and it is not dressed up as either of the others. "I could not tell" is a
    // different fact from "it worked" and from "it failed", and the person has to check the folder
    // either way — reporting it as success hides lost work, reporting it as failure invites a retry
    // that may duplicate files that did land.
    if (unknown.length > 0) {
      const listed = unknown.slice(0, 3).map((u) => u.name).join(", ");
      const more = unknown.length > 3 ? ` +${unknown.length - 3}` : "";
      toast.error(t("transfer.unknown", { n: unknown.length, list: listed + more }));
    } else if (failed.length > 0) {
      // The names matter: "3 failed" alone sends the person hunting through a folder for which
      // three. Up to three are named, and the rest are counted.
      const listed = failed.slice(0, 3).map((f) => `${f.name} (${f.code})`).join(", ");
      const more = failed.length > 3 ? ` +${failed.length - 3}` : "";
      toast.error(
        t("transfer.partial", {
          done: okTotal.current,
          failed: failed.length,
          list: listed + more,
        }),
      );
    } else if (okTotal.current > 0) {
      toast.info(
        op === "copy"
          ? t("transfer.copied", { n: okTotal.current })
          : t("transfer.moved", { n: okTotal.current }),
      );
    }
    onClose();
  };

  /** run sends items in chunks and folds the outcome into the running totals. */
  const run = async (items: FileNode[], overwrite: boolean) => {
    setStep("run");
    stopped.current = false;
    setProgress({ done: 0, total: items.length });

    // Collected locally, not in state: state set inside this closure is not visible to the check
    // below, which would then decide "no conflicts" for every batch.
    const clashes: FileNode[] = [];
    let processed = 0;

    for (let i = 0; i < items.length; i += TRANSFER_CHUNK) {
      if (stopped.current) break;
      const slice = items.slice(i, i + TRANSFER_CHUNK);

      try {
        const res = await api.batch({
          op,
          overwrite,
          items: slice.map((e) => ({ from: e.path, to: targetFor(e) })),
        });
        res.results.forEach((r, j) => {
          const entry = slice[j];
          if (r.ok) {
            okTotal.current += 1;
            return;
          }
          // A conflict is not a failure: it is the question "replace it?" arriving as data.
          if (r.code === "target_exists" && !overwrite && entry) {
            clashes.push(entry);
            return;
          }
          failTotal.current.push({ name: entry?.name ?? r.from, code: r.code ?? "failed" });
        });
      } catch (err) {
        // The request failed as a whole. What that means depends on whether the server answered:
        //
        //   answered (4xx/5xx) — the batch was decided. The endpoint validates op/items before it
        //   runs anything and answers per ITEM otherwise, so a status here is a refusal of the whole
        //   chunk, and every item in it is known to have failed (or to have collided).
        //
        //   no answer (network, timeout, unparsable body) — the items MAY have been applied. The
        //   response cannot say which, so the honest state is "unknown". Marking them done invents
        //   success; marking only the first one failed invents a specific failure for its 24
        //   neighbours. Neither is a fact, and the person acts on both.
        const answered = err instanceof APIError && err.status >= 400;
        const code = err instanceof APIError ? err.code : "network";

        if (answered) {
          // With exactly one item the server answers that item's own status instead of a list, so a
          // single-file transfer keeps the same semantics as every other call.
          for (const entry of slice) {
            if (code === "target_exists" && !overwrite) {
              clashes.push(entry);
            } else {
              failTotal.current.push({ name: entry.name, code });
            }
          }
        } else {
          for (const entry of slice) unknownTotal.current.push({ name: entry.name });
          // Stop: every further chunk would add unknowns for the same reason, and the request that
          // would establish the truth is the one that just failed.
          stopped.current = true;
        }
      }

      processed += slice.length;
      setProgress({ done: processed, total: items.length });
    }

    if (clashes.length > 0) {
      setConflicts(clashes);
      setStep("conflict");
      return;
    }
    finish();
  };

  /** begin resets the totals and starts the first run. */
  const begin = () => {
    okTotal.current = 0;
    failTotal.current = [];
    unknownTotal.current = [];
    setConflicts([]);
    void run(entries, false);
  };

  /** replaceAll retries only the items that collided, with overwrite granted. */
  const replaceAll = () => {
    const retry = conflicts;
    setConflicts([]);
    void run(retry, true);
  };

  if (step === "conflict") {
    const shown = conflicts.slice(0, 5).map((e) => e.name).join(", ");
    const more = conflicts.length > 5 ? ` +${conflicts.length - 5}` : "";
    return (
      <Modal
        title={t("transfer.exists", { n: conflicts.length })}
        description={t("transfer.replaceBody")}
        onClose={finish}
        footer={
          <>
            <button type="button" className="btn" onClick={finish}>
              {t("dlg.cancel")}
            </button>
            <button type="button" className="btn primary" onClick={replaceAll}>
              {t("transfer.replace")}
            </button>
          </>
        }
      >
        <div className="mono">{shown + more}</div>
        <p className="hintline">{t("transfer.keeping")}</p>
      </Modal>
    );
  }

  if (step === "run") {
    const label = t("transfer.working", { done: progress.done, total: progress.total });
    return (
      <Modal
        title={op === "copy" ? t("transfer.copy") : t("transfer.move")}
        onClose={() => {
          stopped.current = true;
        }}
        footer={
          <button
            type="button"
            className="btn"
            onClick={() => {
              stopped.current = true;
            }}
          >
            {t("transfer.stop")}
          </button>
        }
      >
        <p className="hintline">{label}</p>
        <progress className="meter" max={progress.total} value={progress.done} aria-label={label} />
      </Modal>
    );
  }

  return (
    <Modal
      title={op === "copy" ? t("transfer.copy") : t("transfer.move")}
      description={t("transfer.choose")}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="btn" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="btn primary" onClick={begin}>
            {op === "copy" ? t("transfer.copyHere") : t("transfer.moveHere")}
          </button>
        </>
      }
    >
      <div className="aclbar">
        {dir ? (
          <button
            type="button"
            className="btn sm"
            onClick={() => setDir(dir.slice(0, dir.lastIndexOf("/")))}
          >
            {t("files.up")}
          </button>
        ) : null}
        <span className="mono">{dir ? `/${dir}` : "/"}</span>
      </div>

      {listError ? (
        <p className="hintline">{listError}</p>
      ) : folders === null ? (
        <p className="hintline">{t("state.loading")}</p>
      ) : folders.length === 0 ? (
        <p className="hintline">{t("transfer.empty")}</p>
      ) : (
        <div className="filelist-simple">
          {folders.map((folder) => (
            <div className="li" key={folder.id}>
              <span className="n">{folder.name}</span>
              <button
                type="button"
                className="btn sm"
                onClick={() => setDir(dir ? `${dir}/${folder.name}` : folder.name)}
              >
                <Icon name="chevron" size={11} />
              </button>
            </div>
          ))}
        </div>
      )}
    </Modal>
  );
}