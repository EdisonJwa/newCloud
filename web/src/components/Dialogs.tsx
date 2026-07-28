// Dialogs.
//
// Native <dialog> with showModal(): the platform provides the top layer, focus containment,
// Escape handling and the inert backdrop, none of which is worth reimplementing. What is
// added here is the form plumbing and the one thing the platform does not do — returning
// focus to the element that opened the dialog.

import { useEffect, useRef, useState, type ReactNode } from "react";
import { useI18n } from "../i18n";
import { useToast } from "../ui";
import { api, APIError } from "../api";
import type { FileNode } from "../types";

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

export function ShareDialog({ entry, onClose }: { entry: FileNode; onClose: () => void }) {
  const { t } = useI18n();
  const toast = useToast();
  const [kind, setKind] = useState<"link" | "request">("link");
  const [password, setPassword] = useState("");
  const [hours, setHours] = useState("");
  const [maxUses, setMaxUses] = useState("");
  const [busy, setBusy] = useState(false);
  const [created, setCreated] = useState<string | null>(null);

  const submit = async () => {
    setBusy(true);
    try {
      const body: Parameters<typeof api.createShare>[0] = { path: entry.path, kind };
      if (password) body.password = password;
      const h = Number.parseInt(hours, 10);
      if (h > 0) body.expires_in_seconds = h * 3600;
      const m = Number.parseInt(maxUses, 10);
      if (m > 0) {
        if (kind === "request") body.max_uploads = m;
        else body.max_downloads = m;
      }

      const res = await api.createShare(body);
      const url = `${location.origin}/s/${res.token}`;
      await navigator.clipboard.writeText(url).catch(() => {});
      setCreated(url);
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
        <div className="copybox">{created}</div>
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
          <button type="button" className="btn primary" disabled={busy} onClick={submit}>
            {t("dlg.createLink")}
          </button>
        </>
      }
    >
      <label className="field">
        <span className="micro">{t("dlg.shareKind")}</span>
        <select value={kind} onChange={(e) => setKind(e.target.value as "link" | "request")}>
          <option value="link">{t("dlg.shareLink")}</option>
          {entry.kind === "dir" ? <option value="request">{t("dlg.shareRequest")}</option> : null}
        </select>
      </label>
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
