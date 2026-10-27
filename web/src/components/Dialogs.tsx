// Dialogs.
//
// Native `<dialog>` with `showModal()`: the platform provides the top layer, focus containment,
// Escape handling and the inert backdrop, none of which is worth reimplementing. What is added
// here is the form plumbing, the one thing the platform does not do — returning focus to the
// element that opened the dialog — and the validation that stops a submit the server would
// reject anyway.
//
// Every dialog follows the same shell (`serein-dialog` with a header, body and footer) so their
// rhythm and their Escape behaviour cannot drift apart, and the footer always puts the
// committing action last and on the right, which is where a person's eye already is.

import { useEffect, useId, useRef, useState, type ReactNode } from "react";
import { useI18n } from "../i18n";
import { Icon, useToast } from "../ui";
import { api, APIError } from "../api";
import type { CreateShareRequest, FileNode, Share } from "../types";

/* ------------------------------------------------------------------ shell --- */

interface ShellProps {
  title: string;
  description?: string;
  children?: ReactNode;
  footer: ReactNode;
  onClose: () => void;
  size?: "sm" | "normal" | "wide";
  /** Shown beside the title; a dialog that confirms a destructive act says so in words. */
  tone?: "neutral" | "danger";
}

/**
 * Dialog is the shell every dialog uses.
 *
 * `open` is driven by mounting rather than by a boolean prop: a dialog that stays mounted and
 * is hidden loses its native top-layer behaviour on some browsers, and the focus return has to
 * happen exactly once on unmount regardless of which control closed it.
 */
function Dialog({ title, description, children, footer, onClose, size = "normal", tone = "neutral" }: ShellProps) {
  const { t } = useI18n();
  const ref = useRef<HTMLDialogElement>(null);
  const opener = useRef<Element | null>(null);
  const base = useId().replace(/[^a-zA-Z0-9]/g, "");
  const titleId = `dlg-${base}-title`;
  const descId = description ? `dlg-${base}-desc` : undefined;

  useEffect(() => {
    opener.current = document.activeElement;
    ref.current?.showModal();
    return () => {
      // Without this the next Tab starts from the top of the page and a keyboard user loses
      // their place after every dialog.
      if (opener.current instanceof HTMLElement) opener.current.focus();
    };
  }, []);

  return (
    <dialog
      ref={ref}
      className={`serein-dialog${size === "sm" ? " serein-dialog--sm" : size === "wide" ? " serein-dialog--wide" : ""}${tone === "danger" ? " serein-dialog--alert" : ""}`}
      role={tone === "danger" ? "alertdialog" : undefined}
      aria-labelledby={titleId}
      aria-describedby={descId}
      onCancel={(e) => {
        e.preventDefault();
        onClose();
      }}
    >
      <div className="serein-dialog__header">
        <h2 className="serein-dialog__title" id={titleId}>
          {title}
        </h2>
        {description ? (
          <p className="serein-dialog__description" id={descId}>
            {description}
          </p>
        ) : null}
      </div>
      {children ? <div className="serein-dialog__body">{children}</div> : null}
      <div className="serein-dialog__footer">{footer}</div>
      <button
        type="button"
        className="serein-icon-btn serein-icon-btn--sm serein-dialog__close"
        aria-label={t("dlg.close")}
        onClick={onClose}
      >
        <Icon name="close" size={13} />
      </button>
    </dialog>
  );
}

function errText(err: unknown): string {
  return err instanceof APIError ? err.message || err.code : String(err);
}

/** Field wraps a label, control and its hint/error in Serein's field chrome. */
function Field({
  label,
  hint,
  error,
  children,
  optional,
  htmlFor,
}: {
  label: string;
  hint?: string;
  error?: string;
  children: ReactNode;
  optional?: string;
  htmlFor?: string;
}) {
  return (
    <div className={`serein-field${error ? " is-invalid" : ""}`}>
      <div className="serein-field__label-row">
        <label className="serein-label" htmlFor={htmlFor}>
          {label}
        </label>
        {optional ? <span className="serein-optional">{optional}</span> : null}
      </div>
      {children}
      {error ? (
        <span className="serein-field__error" role="alert">
          <Icon name="alert" size={12} />
          {error}
        </span>
      ) : hint ? (
        <span className="serein-hint">{hint}</span>
      ) : null}
    </div>
  );
}

/* ---------------------------------------------------------------- actions --- */

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
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    // A dialog whose only field is not focused is a dialog the person has to aim at.
    inputRef.current?.focus();
  }, []);

  const trimmed = name.trim();
  const invalid = trimmed.length > 0 && /[\\/:*?"<>|]/.test(trimmed);

  const submit = async () => {
    if (!trimmed || invalid) return;
    setBusy(true);
    try {
      await api.mkdir(dir ? `${dir}/${trimmed}` : trimmed);
      toast.success(t("toast.folderCreated"));
      onDone();
      onClose();
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={t("dlg.newFolder")}
      description={dir ? `/${dir}` : t("files.root")}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="serein-btn serein-btn--quiet" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button
            type="button"
            className="serein-btn"
            disabled={busy || !trimmed || invalid}
            onClick={() => void submit()}
          >
            {t("dlg.create")}
          </button>
        </>
      }
    >
      <Field
        label={t("dlg.folderLabel")}
        htmlFor="new-folder-name"
        error={invalid ? t("dlg.nameInvalid") : undefined}
        hint={invalid ? undefined : t("dlg.nameHint")}
      >
        <div className="serein-input-wrap">
          <input
            ref={inputRef}
            id="new-folder-name"
            className="serein-input"
            value={name}
            autoComplete="off"
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submit();
            }}
          />
        </div>
      </Field>
    </Dialog>
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
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    const el = inputRef.current;
    if (!el) return;
    el.focus();
    // Selecting everything except the extension is the small kindness that makes rename
    // usable: retyping a name should not mean retyping ".jpg".
    const dot = entry.name.lastIndexOf(".");
    el.setSelectionRange(0, dot > 0 ? dot : entry.name.length);
  }, [entry.name]);

  const trimmed = name.trim();
  const unchanged = trimmed === entry.name;
  const invalid = trimmed.length > 0 && /[\\/:*?"<>|]/.test(trimmed);

  const submit = async () => {
    if (!trimmed || unchanged || invalid) {
      onClose();
      return;
    }
    setBusy(true);
    try {
      await api.rename(entry.path, trimmed);
      toast.success(t("toast.renamed"));
      onDone();
      onClose();
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  return (
    <Dialog
      title={t("dlg.rename")}
      description={entry.path}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="serein-btn serein-btn--quiet" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button
            type="button"
            className="serein-btn"
            disabled={busy || !trimmed || unchanged || invalid}
            onClick={() => void submit()}
          >
            {t("dlg.rename")}
          </button>
        </>
      }
    >
      <Field
        label={t("dlg.renameLabel")}
        htmlFor="rename-name"
        error={invalid ? t("dlg.nameInvalid") : undefined}
      >
        <div className="serein-input-wrap">
          <input
            ref={inputRef}
            id="rename-name"
            className="serein-input"
            value={name}
            autoComplete="off"
            onChange={(e) => setName(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submit();
            }}
          />
        </div>
      </Field>
    </Dialog>
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
      toast.success(t("toast.deleted", { n: entries.length }), {
        label: t("toast.undoHint"),
        run: () => toast.info(t("trash.restoreHint")),
      });
    } else {
      // Partial failure is reported rather than swallowed: the person has to know which items
      // are still there.
      toast.error(failures[0] ?? "");
    }
  };

  return (
    <Dialog
      tone="danger"
      size="sm"
      title={
        entries.length === 1
          ? t("dlg.deleteOne", { name: entries[0]?.name ?? "" })
          : t("dlg.deleteMany", { n: entries.length })
      }
      description={t("dlg.deleteBody")}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="serein-btn serein-btn--quiet" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="serein-btn serein-btn--danger" disabled={busy} onClick={() => void submit()}>
            {t("detail.delete")}
          </button>
        </>
      }
    />
  );
}

/* ------------------------------------------------------------------ share --- */

/** The three shapes a link can take. `request` is only offered for a directory. */
type ShareMode = "link" | "email" | "request";

/**
 * parseRecipients turns a pasted list into addresses.
 *
 * People paste addresses separated by commas, semicolons, spaces or newlines, usually with a
 * trailing separator. Accepting all of them is friendlier than demanding one, and it costs
 * nothing: the server re-validates every address with net/mail and is the authority. Addresses
 * are lower-cased and de-duplicated because the stored form is lower-cased — sending both cases
 * would create two recipient rows for one person.
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
 *  internal/share accepts `user@host` without a dot (an intranet address is still an address).
 *  A client rule that demanded a dot would refuse something the server would happily deliver to.
 */
const looksLikeEmail = (addr: string): boolean => /^[^@\s]+@[^@\s]+$/.test(addr);

const EXPIRY_PRESETS: readonly { key: string; hours: number }[] = [
  { key: "share.expiry.never", hours: 0 },
  { key: "share.expiry.day", hours: 24 },
  { key: "share.expiry.week", hours: 24 * 7 },
  { key: "share.expiry.month", hours: 24 * 30 },
];

const USE_PRESETS: readonly { key: string; value: number }[] = [
  { key: "share.uses.unlimited", value: 0 },
  { key: "share.uses.25", value: 25 },
  { key: "share.uses.100", value: 100 },
];

export function ShareDialog({ entry, onClose }: { entry: FileNode; onClose: () => void }) {
  const { t } = useI18n();
  const toast = useToast();
  const [kind, setKind] = useState<ShareMode>("link");
  const [label, setLabel] = useState("");
  const [password, setPassword] = useState("");
  const [hours, setHours] = useState(0);
  const [maxUses, setMaxUses] = useState(0);
  const [recipients, setRecipients] = useState("");
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<{ url: string; recipients: string[]; share: Share | null } | null>(null);
  const [copied, setCopied] = useState(false);

  const addresses = parseRecipients(recipients);
  const invalid = addresses.filter((a) => !looksLikeEmail(a));
  // An email share with no recipients is refused by the server and would be a link that opens
  // for nobody: the kind alone is what makes the recipient check apply, and it has nothing to
  // match.
  const missingRecipients = kind === "email" && addresses.length === 0;
  const canSubmit = !busy && !missingRecipients && invalid.length === 0;

  const submit = async () => {
    setBusy(true);
    try {
      const body: CreateShareRequest = { path: entry.path, kind };
      if (label.trim()) body.label = label.trim();
      if (password) body.password = password;
      if (hours > 0) body.expires_in_seconds = hours * 3600;
      if (maxUses > 0) {
        if (kind === "request") body.max_uploads = maxUses;
        else body.max_downloads = maxUses;
      }
      if (kind === "email") body.recipients = addresses;

      const res = await api.createShare(body);
      const url = `${location.origin}/s/${res.token}`;
      // Copy is best-effort, but "copied" must not claim a copy that failed: the one-shot
      // link is only shown in this dialog, and a user who trusts a false "copied" loses it.
      await navigator.clipboard.writeText(url).then(
        () => setCopied(true),
        () => {
          /* the dialog still shows the URL to copy by hand */
        },
      );
      setCreated({ url, recipients: res.share?.recipients ?? [], share: res.share ?? null });
      setBusy(false);
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  if (created) {
    return (
      <Dialog
        title={t("dlg.linkReady")}
        description={t("dlg.linkReadyBody")}
        onClose={onClose}
        footer={
          <button type="button" className="serein-btn" onClick={onClose}>
            {t("dlg.done")}
          </button>
        }
      >
        <div className="serein-link-box">
          <span className="serein-link-box__url">{created.url}</span>
          <button
            type="button"
            className="serein-btn serein-btn--sm serein-btn--outline"
            onClick={() => {
              void navigator.clipboard?.writeText(created.url).then(
                () => {
                  setCopied(true);
                  toast.success(t("toast.linkCopied"));
                },
                () => toast.error(t("inspector.copyFailed")),
              );
            }}
          >
            {copied ? t("inspector.copied") : t("inspector.copy")}
          </button>
        </div>
        {created.recipients.length > 0 ? (
          <p className="serein-hint" style={{ marginTop: 10 }}>
            {t("dlg.emailInvited", { list: created.recipients.join(", ") })}
          </p>
        ) : null}
        <p className="serein-hint" style={{ marginTop: 10 }}>
          {t("dlg.tokenOnce")}
        </p>
      </Dialog>
    );
  }

  return (
    <Dialog
      title={t("dlg.shareTitle", { name: entry.name })}
      description={t("dlg.shareDescription")}
      size="normal"
      onClose={onClose}
      footer={
        <>
          <button type="button" className="serein-btn serein-btn--quiet" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="serein-btn" disabled={!canSubmit} onClick={() => void submit()}>
            {t("dlg.createLink")}
          </button>
        </>
      }
    >
      <div className="serein-form-section">
        <div className="serein-target">
          <Icon name={entry.kind === "dir" ? "files" : "doc"} size={16} />
          <div className="serein-target__main">
            <div className="serein-target__name">{entry.name}</div>
            <div className="serein-target__meta">{entry.path}</div>
          </div>
        </div>
      </div>

      <fieldset className="serein-form-section">
        <legend className="serein-form-section__title">{t("dlg.shareKind")}</legend>
        <div className="serein-radio-group">
          <label className="serein-radio">
            <input
              className="serein-radio__input"
              type="radio"
              name="share-kind"
              checked={kind === "link"}
              onChange={() => setKind("link")}
            />
            <span className="serein-radio__dot" aria-hidden="true" />
            <span className="serein-radio__text">
              <span className="serein-radio__label">{t("dlg.kindView")}</span>
              <span className="serein-radio__description">{t("dlg.kindViewHint")}</span>
            </span>
          </label>
          <label className="serein-radio">
            <input
              className="serein-radio__input"
              type="radio"
              name="share-kind"
              checked={kind === "email"}
              onChange={() => setKind("email")}
            />
            <span className="serein-radio__dot" aria-hidden="true" />
            <span className="serein-radio__text">
              <span className="serein-radio__label">{t("dlg.kindEmail")}</span>
              <span className="serein-radio__description">{t("dlg.kindEmailHint")}</span>
            </span>
          </label>
          {entry.kind === "dir" ? (
            <label className="serein-radio">
              <input
                className="serein-radio__input"
                type="radio"
                name="share-kind"
                checked={kind === "request"}
                onChange={() => setKind("request")}
              />
              <span className="serein-radio__dot" aria-hidden="true" />
              <span className="serein-radio__text">
                <span className="serein-radio__label">{t("dlg.kindUpload")}</span>
                <span className="serein-radio__description">{t("dlg.kindUploadHint")}</span>
              </span>
            </label>
          ) : null}
        </div>
      </fieldset>

      {kind === "email" ? (
        <div className="serein-form-section">
          <Field
            label={t("dlg.recipients")}
            hint={t("dlg.recipientsHint")}
            error={
              missingRecipients
                ? t("dlg.recipientsRequired")
                : invalid.length > 0
                  ? t("dlg.recipientsInvalid", { list: invalid.join(", ") })
                  : undefined
            }
            htmlFor="share-recipients"
          >
            <div className="serein-input-wrap">
              <textarea
                id="share-recipients"
                className="serein-textarea"
                value={recipients}
                rows={3}
                placeholder="name@example.com, other@example.org"
                onChange={(e) => setRecipients(e.target.value)}
              />
            </div>
          </Field>
        </div>
      ) : null}

      <div className="serein-form-section">
        <Field label={t("dlg.linkName")} optional={t("dlg.optional")} htmlFor="share-label" hint={t("dlg.linkNameHint")}>
          <div className="serein-input-wrap">
            <input
              id="share-label"
              className="serein-input"
              value={label}
              placeholder={entry.name}
              onChange={(e) => setLabel(e.target.value)}
            />
          </div>
        </Field>
      </div>

      <div className="serein-form-section">
        <Field label={t("dlg.password")} optional={t("dlg.optional")} htmlFor="share-password" hint={t("dlg.passwordHint")}>
          <div className="serein-input-wrap">
            <input
              id="share-password"
              className="serein-input"
              type="password"
              autoComplete="new-password"
              value={password}
              onChange={(e) => setPassword(e.target.value)}
            />
          </div>
        </Field>
      </div>

      <div className="serein-form-section">
        <div className="serein-form-grid">
          <Field label={t("dlg.expires")} htmlFor="share-expiry">
            <div className="serein-select-wrap">
              <select
                id="share-expiry"
                className="serein-select"
                value={hours}
                onChange={(e) => setHours(Number.parseInt(e.target.value, 10) || 0)}
              >
                {EXPIRY_PRESETS.map((p) => (
                  <option key={p.key} value={p.hours}>
                    {t(p.key)}
                  </option>
                ))}
              </select>
            </div>
          </Field>
          <Field label={kind === "request" ? t("dlg.maxUploads") : t("dlg.maxUses")} htmlFor="share-uses">
            <div className="serein-select-wrap">
              <select
                id="share-uses"
                className="serein-select"
                value={maxUses}
                onChange={(e) => setMaxUses(Number.parseInt(e.target.value, 10) || 0)}
              >
                {USE_PRESETS.map((p) => (
                  <option key={p.key} value={p.value}>
                    {t(p.key)}
                  </option>
                ))}
              </select>
            </div>
          </Field>
        </div>
      </div>

      <p className="serein-form-summary">{t("dlg.summary", { kind: t(`dlg.summaryKind.${kind}`) })}</p>
    </Dialog>
  );
}

/* ------------------------------------------------------------ app passwords -- */

export function NewPasswordDialog({ onClose, onDone }: { onClose: () => void; onDone: () => void }) {
  const { t } = useI18n();
  const toast = useToast();
  const [label, setLabel] = useState("");
  const [scope, setScope] = useState("rw");
  const [busy, setBusy] = useState(false);
  const [secret, setSecret] = useState<string | null>(null);
  const [copied, setCopied] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    inputRef.current?.focus();
  }, []);

  const submit = async () => {
    setBusy(true);
    try {
      const res = await api.createAppPassword({ label: label.trim() || "app password", scope });
      setSecret(res.secret);
      setBusy(false);
    } catch (err) {
      toast.error(errText(err));
      setBusy(false);
    }
  };

  if (secret) {
    return (
      <Dialog
        title={t("access.secretTitle")}
        description={t("access.secretBody")}
        onClose={() => {
          onDone();
          onClose();
        }}
        footer={
          <button
            type="button"
            className="serein-btn"
            onClick={() => {
              onDone();
              onClose();
            }}
          >
            {t("dlg.done")}
          </button>
        }
      >
        <div className="serein-link-box">
          <span className="serein-link-box__url">{secret}</span>
          <button
            type="button"
            className="serein-btn serein-btn--sm serein-btn--outline"
            onClick={() => {
              void navigator.clipboard?.writeText(secret).then(
                () => {
                  setCopied(true);
                  toast.success(t("inspector.copied"));
                },
                () => toast.error(t("inspector.copyFailed")),
              );
            }}
          >
            {copied ? t("inspector.copied") : t("inspector.copy")}
          </button>
        </div>
      </Dialog>
    );
  }

  return (
    <Dialog
      title={t("access.newPassword")}
      description={t("access.newPasswordHint")}
      onClose={onClose}
      footer={
        <>
          <button type="button" className="serein-btn serein-btn--quiet" onClick={onClose}>
            {t("dlg.cancel")}
          </button>
          <button type="button" className="serein-btn" disabled={busy} onClick={() => void submit()}>
            {t("dlg.create")}
          </button>
        </>
      }
    >
      <Field label={t("access.label")} htmlFor="pw-label">
        <div className="serein-input-wrap">
          <input
            ref={inputRef}
            id="pw-label"
            className="serein-input"
            value={label}
            placeholder={t("access.labelPlaceholder")}
            onChange={(e) => setLabel(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") void submit();
            }}
          />
        </div>
      </Field>
      <Field label={t("access.scope")} htmlFor="pw-scope" hint={t("access.scopeHint")}>
        <div className="serein-select-wrap">
          <select id="pw-scope" className="serein-select" value={scope} onChange={(e) => setScope(e.target.value)}>
            <option value="rw">{t("access.scopeRW")}</option>
            <option value="ro">{t("access.scopeRO")}</option>
          </select>
        </div>
      </Field>
    </Dialog>
  );
}

/* ------------------------------------------------------------- copy / move --- */

export type TransferOp = "copy" | "move";

/** Chunk size for one batch request.
 *
 *  Not a server limit — the endpoint accepts up to 200 items — but the granularity at which
 *  progress becomes visible and Stop becomes meaningful. Twenty-five is roughly a screenful.
 */
const TRANSFER_CHUNK = 25;

/**
 * TransferDialog copies or moves a selection into a folder the person picks.
 *
 * Three steps in one dialog because they are one task: choose the destination, watch it happen
 * (and be able to stop it), and answer the conflict question if one comes up. Three separate
 * dialogs would turn a two-second operation into a wizard.
 *
 * Progress and Stop are real rather than decorative: the items go in chunks, so the counter
 * advances as each batch completes and Stop ends the run between them. What already succeeded
 * stays — the alternative is a rollback of a partial operation, which this API deliberately
 * does not promise.
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

  // Totals and the stop flag live in refs, not state: the retry after a conflict is a second run
  // of the same transfer, and resetting plain state there would make the final message report
  // only the retried items. The stop flag must also be readable inside the await loop, where
  // state is stale.
  const stopped = useRef(false);
  const okTotal = useRef(0);
  const failTotal = useRef<{ name: string; code: string }[]>([]);
  // Items whose outcome the client genuinely does not know. See the catch in run(): a request
  // that failed as a whole may have been applied in part, and the response cannot say which
  // items made it.
  const unknownTotal = useRef<{ name: string }[]>([]);

  // The destination is a folder, so the browser lists directories only: showing files would
  // invite the reading "put it inside this file".
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

  const run = async (replace: boolean) => {
    setStep("run");
    stopped.current = false;
    let ok = okTotal.current;
    let fail = [...failTotal.current];
    let unknown = [...unknownTotal.current];
    // A snapshot of the queue: `stopped` is checked inside the loop, not while filtering, so a
    // Stop pressed between two chunks cannot change which items were queued.
    const queue = entries.slice();

    for (let i = 0; i < queue.length; i += TRANSFER_CHUNK) {
      if (stopped.current) break;
      const chunk = queue.slice(i, i + TRANSFER_CHUNK);
      try {
        const res = await api.batch({
          op,
          from: chunk.map((e) => e.path),
          to: dir,
          replace,
        } as never);
        for (const r of res.results ?? []) {
          if (r.error) {
            if (r.code === "target_exists") continue;
            fail.push({ name: r.from ?? "", code: r.code ?? r.error });
          } else {
            ok += 1;
          }
        }
      } catch (err) {
        // A failed chunk may have been partly applied: every item in it is "unknown", not
        // "failed", and the message says so rather than claiming nothing happened.
        for (const e of chunk) unknown.push({ name: e.name });
        if (err instanceof APIError && err.code === "target_exists") {
          setConflicts(chunk);
          setStep("conflict");
          okTotal.current = ok;
          failTotal.current = fail;
          unknownTotal.current = unknown;
          setProgress({ done: ok + fail.length, total: entries.length });
          return;
        }
      }
      okTotal.current = ok;
      failTotal.current = fail;
      unknownTotal.current = unknown;
      setProgress({ done: ok + fail.length, total: entries.length });
    }

    okTotal.current = ok;
    failTotal.current = fail;
    unknownTotal.current = unknown;
    setStep("pick");
    onDone();
    onClose();

    if (unknown.length > 0) {
      toast.error(
        t("transfer.unknown", {
          n: unknown.length,
          list: unknown
            .slice(0, 3)
            .map((u) => u.name)
            .join(", "),
        }),
      );
    } else if (fail.length > 0) {
      toast.error(
        t("transfer.partial", { done: ok, failed: fail.length, list: fail[0]?.name ?? "" }),
      );
    } else {
      toast.success(t(op === "copy" ? "transfer.copied" : "transfer.moved", { n: ok }));
    }
  };

  const parent = dir.includes("/") ? dir.slice(0, dir.lastIndexOf("/")) : "";

  return (
    <Dialog
      title={op === "copy" ? t("transfer.copyTitle") : t("transfer.moveTitle")}
      description={t("transfer.subtitle", { n: entries.length })}
      size="wide"
      onClose={onClose}
      footer={
        step === "run" ? (
          <button
            type="button"
            className="serein-btn serein-btn--quiet"
            onClick={() => {
              stopped.current = true;
            }}
          >
            {t("transfer.stop")}
          </button>
        ) : step === "conflict" ? (
          <>
            <button type="button" className="serein-btn serein-btn--quiet" onClick={onClose}>
              {t("dlg.cancel")}
            </button>
            <button type="button" className="serein-btn" onClick={() => void run(true)}>
              {t("transfer.replace")}
            </button>
          </>
        ) : (
          <>
            <button type="button" className="serein-btn serein-btn--quiet" onClick={onClose}>
              {t("dlg.cancel")}
            </button>
            <button
              type="button"
              className="serein-btn"
              disabled={entries.length === 0}
              onClick={() => void run(false)}
            >
              {op === "copy" ? t("transfer.copyHere") : t("transfer.moveHere")}
            </button>
          </>
        )
      }
    >
      {step === "conflict" ? (
        <>
          <p className="serein-hint">{t("transfer.exists", { n: conflicts.length })}</p>
          <p className="serein-hint">{t("transfer.replaceBody")}</p>
          <p className="serein-hint">{t("transfer.keeping")}</p>
        </>
      ) : step === "run" ? (
        <>
          <p className="serein-hint">{t("transfer.working", { done: progress.done, total: progress.total })}</p>
          <div className="serein-meter" role="progressbar" aria-valuemin={0} aria-valuemax={progress.total} aria-valuenow={progress.done}>
            <span
              className="serein-meter__bar"
              style={{ width: `${progress.total > 0 ? Math.round((progress.done / progress.total) * 100) : 0}%` }}
            />
          </div>
        </>
      ) : (
        <>
          <div className="wc-picker__crumbs">
            <button type="button" className="serein-breadcrumb__link" onClick={() => setDir("")}>
              {t("files.root")}
            </button>
            {dir
              ? dir.split("/").filter(Boolean).map((part, i, all) => {
                  const path = all.slice(0, i + 1).join("/");
                  return (
                    <span key={path} className="serein-breadcrumb__item">
                      <span className="serein-breadcrumb__sep" aria-hidden="true">
                        <Icon name="chevron" size={10} />
                      </span>
                      <button type="button" className="serein-breadcrumb__link" onClick={() => setDir(path)}>
                        {part}
                      </button>
                    </span>
                  );
                })
              : null}
          </div>

          <div className="wc-picker" role="listbox" aria-label={t("transfer.choose")}>
            {parent !== dir ? (
              <button type="button" className="wc-picker__row" onClick={() => setDir(parent)}>
                <Icon name="arrowUp" size={13} />
                {t("files.up")}
              </button>
            ) : null}
            {listError ? (
              <p className="serein-field__error">{listError}</p>
            ) : folders === null ? (
              <p className="serein-hint">{t("state.loading")}</p>
            ) : folders.length === 0 ? (
              <p className="serein-hint">{t("transfer.empty")}</p>
            ) : (
              folders.map((folder) => (
                <button
                  key={folder.id}
                  type="button"
                  className="wc-picker__row"
                  onClick={() => setDir(folder.path)}
                >
                  <Icon name="files" size={13} />
                  <span className="wc-grow wc-truncate">{folder.name}</span>
                  <Icon name="chevron" size={12} />
                </button>
              ))
            )}
          </div>

          <p className="serein-hint">{t("transfer.destination", { path: dir ? `/${dir}` : t("files.root") })}</p>
        </>
      )}
    </Dialog>
  );
}
