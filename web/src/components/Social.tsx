// The three social surfaces: stars, labels and discussions (core-scope §4, §5, §6).
//
// They live together because they answer one question ??"what has been said about this file" ??and
// because each of them is small enough that a file per feature would be three files of imports and
// one component each. What they share is a rule rather than code:
//
//   **Nothing here decides who may do what.** `can_moderate` comes from the server with the thread,
//   the tag list comes from the server already filtered to what this caller can see, and a star is a
//   row the server owns. A component that inferred permission from the node's other flags would be a
//   second answer to a question the authorizer already answers, and the two would eventually differ.
//
// The visual vocabulary is deliberately small: a chip is a tag, a filled star is a favorite, a
// bubble is a comment. No component invents a new colour to mean something.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { api, APIError, type Comment, type CommentThread, type Tag } from "../api";
import { useI18n } from "../i18n";
import { Icon, useToast } from "../ui";

/* ------------------------------------------------------------------ stars --- */

/**
 * StarButton toggles the viewer's own favorite.
 *
 * Optimistic, with a rollback: a star is a single bit the user expects to respond instantly, and a
 * round trip that takes 200ms makes the click feel dropped. If the server refuses, the star goes
 * back and the toast says why ??the alternative (disable the button, then enable it) makes every
 * click cost a visible flicker.
 *
 * The id form is used when there is one and the path form otherwise, because a file inside somebody
 * else's namespace has no path this caller can spell. §4 requires both for exactly that reason.
 */
export function StarButton({
  node,
  starred,
  onChange,
  compact = false,
}: {
  node: { id: string; path?: string };
  starred: boolean;
  onChange: (starred: boolean) => void;
  compact?: boolean;
}) {
  const { t } = useI18n();
  const toast = useToast();
  const [busy, setBusy] = useState(false);
  const [on, setOn] = useState(starred);

  // The prop is authoritative when it changes: a reload after another surface starred the same file
  // must not leave this button disagreeing with the row beside it.
  useEffect(() => setOn(starred), [starred]);

  const toggle = async () => {
    if (busy) return;
    const next = !on;
    setOn(next);
    onChange(next);
    setBusy(true);
    try {
      const ref = { id: node.id };
      if (next) await api.star(ref);
      else await api.unstar(ref);
    } catch (err) {
      setOn(!next);
      onChange(!next);
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <button
      type="button"
      className={`wc-star${on ? " wc-star--on" : ""}${compact ? " wc-star--compact" : ""}`}
      aria-pressed={on}
      aria-label={on ? t("social.unstar") : t("social.star")}
      title={on ? t("social.unstar") : t("social.star")}
      onClick={() => void toggle()}
    >
      <Icon name={on ? "starFill" : "star"} size={compact ? 12 : 14} />
      {compact ? null : <span>{on ? t("social.starred") : t("social.star")}</span>}
    </button>
  );
}

/* ------------------------------------------------------------------- tags --- */

/**
 * TagChips renders a node's labels.
 *
 * `max` truncates with a count rather than a scroll: a row with nine chips is a row whose file name
 * is invisible, and the count tells the user there is more without them having to find out where.
 */
export function TagChips({
  tags,
  max,
  onRemove,
  onClick,
}: {
  tags: Tag[] | undefined;
  max?: number;
  onRemove?: (tag: Tag) => void;
  onClick?: (tag: Tag) => void;
}) {
  const { t } = useI18n();
  if (!tags || tags.length === 0) return null;

  const shown = max !== undefined && tags.length > max ? tags.slice(0, max) : tags;
  const hidden = tags.length - shown.length;

  return (
    <span className="wc-tagchips">
      {shown.map((tag) => {
        const label = (
          <>
            <span className="wc-tagchip__dot" aria-hidden="true" />
            {tag.name}
          </>
        );
        // A chip with a remove handler is a control inside another control, so it is two elements
        // rather than a nested button: nesting one button in another is invalid HTML and browsers
        // disagree about which one the click belongs to.
        return (
          <span key={tag.id} className={`wc-tagchip wc-tagchip--${tag.color ?? "gray"}`}>
            {onClick ? (
              <button
                type="button"
                className="wc-tagchip__body"
                onClick={(e) => {
                  e.stopPropagation();
                  onClick(tag);
                }}
              >
                {label}
              </button>
            ) : (
              <span className="wc-tagchip__body">{label}</span>
            )}
            {onRemove ? (
              <button
                type="button"
                className="wc-tagchip__x"
                aria-label={t("social.removeTag", { name: tag.name })}
                onClick={(e) => {
                  e.stopPropagation();
                  onRemove(tag);
                }}
              >
                <Icon name="close" size={9} />
              </button>
            ) : null}
          </span>
        );
      })}
      {hidden > 0 ? (
        <span className="wc-tagchip wc-tagchip--more" title={tags.map((x) => x.name).join(", ")}>
          +{hidden}
        </span>
      ) : null}
    </span>
  );
}

/**
 * TagEditor is the assignment control in the inspector.
 *
 * The palette is fixed and comes from the SERVER rather than being listed here: `TagColors` is the
 * set the server validates against, and a client-side copy would drift the day a colour is added.
 * The listing already returns every tag with its colour, so the picker is built from real data.
 */
export function TagEditor({ node }: { node: { id: string; tags?: Tag[] } }) {
  const { t } = useI18n();
  const toast = useToast();
  const [all, setAll] = useState<Tag[] | null>(null);
  const [open, setOpen] = useState(false);
  const [busy, setBusy] = useState(false);
  const [name, setName] = useState("");
  const [error, setError] = useState<string | null>(null);
  const [assigned, setAssigned] = useState<Tag[]>(node.tags ?? []);
  const boxRef = useRef<HTMLDivElement>(null);

  useEffect(() => setAssigned(node.tags ?? []), [node.tags]);

  const load = useCallback(() => {
    setError(null);
    api
      .tags()
      .then((res) => setAll(res.items ?? []))
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)));
  }, []);

  // Loaded when the picker opens rather than with the inspector: a panel that fetches the whole tag
  // vocabulary for every file clicked pays for a list nobody looked at.
  useEffect(() => {
    if (open && all === null) load();
  }, [open, all, load]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: MouseEvent) => {
      if (boxRef.current && !boxRef.current.contains(e.target as Node)) setOpen(false);
    };
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") setOpen(false);
    };
    document.addEventListener("mousedown", onDown);
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("mousedown", onDown);
      document.removeEventListener("keydown", onKey);
    };
  }, [open]);

  const assignedIDs = useMemo(() => new Set(assigned.map((x) => x.id)), [assigned]);
  const available = useMemo(
    () => (all ?? []).filter((x) => !assignedIDs.has(x.id)),
    [all, assignedIDs],
  );

  const apply = async (tag: Tag) => {
    setBusy(true);
    try {
      const res = await api.assignTag(node.id, tag.id);
      setAssigned(res.tags ?? []);
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setBusy(false);
    }
  };

  const remove = async (tag: Tag) => {
    setBusy(true);
    try {
      const res = await api.unassignTag(node.id, tag.id);
      setAssigned(res.tags ?? []);
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setBusy(false);
    }
  };

  const create = async () => {
    const trimmed = name.trim();
    if (!trimmed) return;
    setBusy(true);
    try {
      const tag = await api.createTag({ name: trimmed });
      setName("");
      // The new tag is assigned in the same gesture: a user who types a label and presses Enter
      // means "label this file", not "add a label to my vocabulary and then do it again".
      const res = await api.assignTag(node.id, tag.id);
      setAssigned(res.tags ?? []);
      setAll((prev) => (prev ? [...prev, tag] : [tag]));
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="wc-tageditor" ref={boxRef}>
      <div className="wc-tageditor__row">
        <TagChips tags={assigned} onRemove={(tag) => void remove(tag)} />
        <button
          type="button"
          className="serein-btn serein-btn--sm serein-btn--quiet wc-tageditor__add"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
          disabled={busy}
        >
          <Icon name="plus" size={11} />
          {assigned.length === 0 ? t("social.addTag") : t("social.editTags")}
        </button>
      </div>

      {open ? (
        <div className="wc-tageditor__panel" role="group" aria-label={t("social.tags")}>
          <form
            className="wc-tageditor__new"
            onSubmit={(e) => {
              e.preventDefault();
              void create();
            }}
          >
            <input
              className="serein-input"
              value={name}
              maxLength={64}
              placeholder={t("social.newTagPlaceholder")}
              aria-label={t("social.newTag")}
              onChange={(e) => setName(e.target.value)}
              disabled={busy}
            />
            <button
              type="submit"
              className="serein-btn serein-btn--sm"
              disabled={busy || name.trim().length === 0}
            >
              {t("social.create")}
            </button>
          </form>

          {error ? <p className="serein-hint wc-tageditor__error">{error}</p> : null}

          {available.length > 0 ? (
            <div className="wc-chips">
              {available.map((tag) => (
                <button
                  key={tag.id}
                  type="button"
                  className={`wc-tagchip wc-tagchip--${tag.color ?? "gray"} wc-tagchip--pick`}
                  disabled={busy}
                  onClick={() => void apply(tag)}
                >
                  <span className="wc-tagchip__dot" aria-hidden="true" />
                  {tag.name}
                  {typeof tag.count === "number" && tag.count > 0 ? (
                    <span className="wc-tagchip__count">{tag.count}</span>
                  ) : null}
                </button>
              ))}
            </div>
          ) : all === null ? null : (
            <p className="serein-hint">{t("social.noMoreTags")}</p>
          )}
        </div>
      ) : null}
    </div>
  );
}

/* --------------------------------------------------------------- comments --- */

/**
 * CommentThread is a node's discussion.
 *
 * Two structural facts the rendering has to honour:
 *
 *   * a deleted comment is a TOMBSTONE with an empty body ??the row stays so the replies keep their
 *     place, so the component shows "removed" rather than skipping it; and
 *   * replies are one level deep, so the reply box is opened on a specific top-level comment and the
 *     child carries its parent's id.
 *
 * `can_moderate` arrives with the thread. A remove button on somebody else's comment is drawn only
 * when the server said this caller may, because a control that answers 404 when pressed is worse
 * than one that is not there.
 */
export function CommentThread({ nodeID, me }: { nodeID: string; me: { id?: string; display_name?: string } | null }) {
  const { t } = useI18n();
  const toast = useToast();
  const [thread, setThread] = useState<CommentThread | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [draft, setDraft] = useState("");
  const [replyTo, setReplyTo] = useState<string | null>(null);
  const [posting, setPosting] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [editDraft, setEditDraft] = useState("");
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [loadingMore, setLoadingMore] = useState(false);

  const load = useCallback(() => {
    setLoading(true);
    setError(null);
    api
      .comments(nodeID)
      .then((res) => {
        setThread(res);
        setCursor(res.cursor || undefined);
      })
      .catch((err) => setError(err instanceof APIError ? err.message || err.code : String(err)))
      .finally(() => setLoading(false));
  }, [nodeID]);

  useEffect(load, [load]);

  const more = async () => {
    if (!cursor || loadingMore) return;
    setLoadingMore(true);
    try {
      const res = await api.comments(nodeID, cursor);
      setThread((prev) =>
        prev
          ? {
              ...res,
              // `can_moderate` and `truncated` describe the node, not the page: taking them from the
              // new page is deliberate, and taking `items` from it is the point.
              items: [...(prev.items ?? []), ...(res.items ?? [])],
            }
          : res,
      );
      setCursor(res.cursor || undefined);
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setLoadingMore(false);
    }
  };

  const post = async () => {
    const body = draft.trim();
    if (!body || posting) return;
    setPosting(true);
    try {
      await api.comment(nodeID, body, replyTo ?? undefined);
      setDraft("");
      setReplyTo(null);
      load();
    } catch (err) {
      // A rate limit is the interesting refusal: the server names it `comment_rate_limited` and the
      // client shows the server's own sentence rather than inventing "try again later".
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    } finally {
      setPosting(false);
    }
  };

  const saveEdit = async (id: string) => {
    const body = editDraft.trim();
    if (!body) return;
    try {
      await api.editComment(id, body);
      setEditing(null);
      setEditDraft("");
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    }
  };

  const remove = async (id: string) => {
    try {
      await api.deleteComment(id);
      load();
    } catch (err) {
      toast.error(err instanceof APIError ? err.message || err.code : String(err));
    }
  };

  const items = thread?.items ?? [];
  const canModerate = thread?.can_moderate ?? false;
  const top = items.filter((c) => !c.parent_id);
  const repliesOf = (id: string) => items.filter((c) => c.parent_id === id);

  if (loading) return <p className="serein-hint">{t("social.loadingComments")}</p>;
  if (error) {
    return (
      <div className="wc-comments__error">
        <p className="serein-hint">{error}</p>
        <button type="button" className="serein-btn serein-btn--sm serein-btn--outline" onClick={load}>
          {t("state.retry")}
        </button>
      </div>
    );
  }

  const canRemove = (c: Comment) => canModerate || (!!me?.id && c.author_id === me.id);
  const isMine = (c: Comment) => !!me?.id && c.author_id === me.id;

  return (
    <div className="wc-comments">
      {top.length === 0 ? (
        <p className="serein-hint">{t("social.noComments")}</p>
      ) : (
        <ol className="wc-comments__list">
          {top.map((c) => (
            <li key={c.id} className="wc-comment">
              <CommentBody
                comment={c}
                mine={isMine(c)}
                canRemove={canRemove(c)}
                editing={editing === c.id}
                editDraft={editDraft}
                onEditDraft={setEditDraft}
                onStartEdit={() => {
                  setEditing(c.id);
                  setEditDraft(c.body);
                }}
                onCancelEdit={() => setEditing(null)}
                onSaveEdit={() => void saveEdit(c.id)}
                onRemove={() => void remove(c.id)}
                onReply={() => setReplyTo(replyTo === c.id ? null : c.id)}
                replyOpen={replyTo === c.id}
              />
              {repliesOf(c.id).length > 0 ? (
                <ol className="wc-comments__replies">
                  {repliesOf(c.id).map((r) => (
                    <li key={r.id} className="wc-comment wc-comment--reply">
                      <CommentBody
                        comment={r}
                        mine={isMine(r)}
                        canRemove={canRemove(r)}
                        editing={editing === r.id}
                        editDraft={editDraft}
                        onEditDraft={setEditDraft}
                        onStartEdit={() => {
                          setEditing(r.id);
                          setEditDraft(r.body);
                        }}
                        onCancelEdit={() => setEditing(null)}
                        onSaveEdit={() => void saveEdit(r.id)}
                        onRemove={() => void remove(r.id)}
                      />
                    </li>
                  ))}
                </ol>
              ) : null}
            </li>
          ))}
        </ol>
      )}

      {thread?.truncated && cursor ? (
        <button
          type="button"
          className="serein-btn serein-btn--sm serein-btn--outline"
          disabled={loadingMore}
          onClick={() => void more()}
        >
          {t("social.moreComments")}
        </button>
      ) : null}

      <form
        className="wc-comments__compose"
        onSubmit={(e) => {
          e.preventDefault();
          void post();
        }}
      >
        {replyTo ? (
          <div className="wc-comments__replying">
            <span>{t("social.replying")}</span>
            <button type="button" className="serein-icon-btn serein-btn--sm" aria-label={t("social.cancelReply")} onClick={() => setReplyTo(null)}>
              <Icon name="close" size={11} />
            </button>
          </div>
        ) : null}
        <textarea
          className="serein-input wc-comments__input"
          rows={3}
          // The server's cap, mirrored into the control. `maxlength` is a courtesy and NOT the
          // enforcement: the server refuses at 2 000 runes regardless, which is what the test pins.
          maxLength={2000}
          value={draft}
          placeholder={replyTo ? t("social.replyPlaceholder") : t("social.commentPlaceholder")}
          aria-label={t("social.commentPlaceholder")}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => {
            // Ctrl/Cmd+Enter posts, which is the convention every discussion box has. Plain Enter
            // inserts a newline: a comment is prose, and stealing Enter makes a two-paragraph reply
            // impossible.
            if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
              e.preventDefault();
              void post();
            }
          }}
          disabled={posting}
        />
        <div className="wc-comments__compose-actions">
          <span className="serein-hint">
            {t("social.commentCount", { n: draft.length, max: 2000 })}
          </span>
          <button
            type="submit"
            className="serein-btn serein-btn--sm"
            disabled={posting || draft.trim().length === 0}
          >
            {replyTo ? t("social.reply") : t("social.post")}
          </button>
        </div>
      </form>
    </div>
  );
}

function CommentBody({
  comment,
  mine,
  canRemove,
  editing,
  editDraft,
  onEditDraft,
  onStartEdit,
  onCancelEdit,
  onSaveEdit,
  onRemove,
  onReply,
  replyOpen,
}: {
  comment: Comment;
  mine: boolean;
  canRemove: boolean;
  editing: boolean;
  editDraft: string;
  onEditDraft: (v: string) => void;
  onStartEdit: () => void;
  onCancelEdit: () => void;
  onSaveEdit: () => void;
  onRemove: () => void;
  onReply?: () => void;
  replyOpen?: boolean;
}) {
  const { t, fmt } = useI18n();

  // A tombstone: the row exists so the replies keep their place, and the body is empty by design
  // rather than by accident. Rendering `comment.body` here would print an empty paragraph that looks
  // like a bug; rendering "removed" says what happened.
  if (comment.deleted) {
    return (
      <div className="wc-comment__body wc-comment__body--gone">
        <Icon name="trash" size={12} />
        <span className="serein-hint">{t("social.commentRemoved")}</span>
        <time className="wc-comment__time">{fmt.relative(comment.created_at)}</time>
      </div>
    );
  }

  return (
    <div className="wc-comment__body">
      <div className="wc-comment__head">
        <span className="wc-comment__author">{comment.author || t("social.unknownAuthor")}</span>
        <time className="wc-comment__time" dateTime={comment.created_at}>
          {fmt.relative(comment.created_at)}
        </time>
        {comment.edited_at ? <span className="serein-hint">{t("social.edited")}</span> : null}
      </div>

      {editing ? (
        <div className="wc-comment__edit">
          <textarea
            className="serein-input"
            rows={2}
            maxLength={2000}
            value={editDraft}
            aria-label={t("social.editComment")}
            onChange={(e) => onEditDraft(e.target.value)}
          />
          <div className="wc-comment__edit-actions">
            <button type="button" className="serein-btn serein-btn--sm" onClick={onSaveEdit}>
              {t("social.save")}
            </button>
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--outline"
              onClick={onCancelEdit}
            >
              {t("social.cancel")}
            </button>
          </div>
        </div>
      ) : (
        // `white-space: pre-line` in the stylesheet, and no dangerouslySetInnerHTML: a comment is
        // user text and is rendered as text. The line breaks are preserved because a comment with a
        // list in it is prose the author formatted.
        <p className="wc-comment__text">{comment.body}</p>
      )}

      {!editing ? (
        <div className="wc-comment__actions">
          {/* Editing is the author's alone ??the server enforces it, and offering the control to a
              moderator would suggest they can change what somebody said. */}
          {mine ? (
            <button type="button" className="serein-btn serein-btn--sm serein-btn--quiet" onClick={onStartEdit}>
              {t("social.edit")}
            </button>
          ) : null}
          {onReply ? (
            <button
              type="button"
              className="serein-btn serein-btn--sm serein-btn--quiet"
              aria-expanded={replyOpen}
              onClick={onReply}
            >
              {t("social.replyAction")}
            </button>
          ) : null}
          {canRemove ? (
            <button type="button" className="serein-btn serein-btn--sm serein-btn--quiet" onClick={onRemove}>
              {t("social.remove")}
            </button>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

/**
 * CommentBadge is the row-level hint that a file has a discussion.
 *
 * It shows a count only when the server gave one, which the listing does not ??so it renders a plain
 * bubble for a node that has comments and nothing otherwise. A zero that means "we did not look" and
 * a zero that means "there are none" are different facts, and only one of them is worth drawing.
 */
export function CommentBadge({ count }: { count?: number }) {
  if (!count) return null;
  return (
    <span className="wc-commentbadge" title={count === 1 ? "1 comment" : `${count} comments`}>
      <Icon name="comment" size={11} />
      {count}
    </span>
  );
}
