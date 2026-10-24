package store

// Social metadata: favorites, tags and comments (core-scope §4, §5, §6).
//
// Three features that share one decision: every row here is keyed by NODE ID, never by path. A
// path is a rendering of where a file currently sits; starring it, labelling it or discussing it
// are statements about the file itself. Keying on the path would make a favorite follow the NAME
// rather than the content, so renaming a folder would silently move somebody else's discussion
// onto a different document.
//
// The second shared decision, which is the one to keep in mind while reading the SQL: NOTHING
// HERE AUTHORISES ANYTHING. A favorites row, a tag assignment and a comment thread are all
// metadata about a node whose access is decided by authz on every request. A stale row therefore
// returns nothing rather than leaking that the node exists — the listing re-checks each node's
// grant, and the comment routes go through the same Authorize call as /api/nodes/{id}.
//
// The third: none of this is hash-chained (core-scope §8). A favorite is not a security-relevant
// action, and putting it in the audit chain would dilute the entries that decide accountability.
// The two exceptions the spec names — moderating somebody else's comment, and admin management of
// a system tag — are audited, and they are audited by their handlers.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Errors specific to this surface. They are separate from the general sentinels so the HTTP layer
// can answer with a code the client acts on rather than a generic conflict.
var (
	// ErrTagLimit is returned when an account or a node has reached its tag cap (§5).
	//
	// Distinct from ErrConflict: a conflict means "that name is taken", this means "there is no
	// room". A client that retries a conflict with a different name succeeds; retrying this one
	// does not, and the user has to remove a tag first.
	ErrTagLimit = errors.New("store: tag limit reached")
	// ErrCommentRateLimited is returned when an author has posted their allowance inside the
	// window (§6).
	//
	// The anonymous-abuse lesson from the share OTP endpoint applies to every user-writable append
	// surface: a table that anybody can write to without a bound is a table that grows until
	// somebody notices.
	ErrCommentRateLimited = errors.New("store: comment rate exceeded")
	// ErrInvalidSocialValue covers the value-level refusals: a tag name that is empty or too long,
	// a colour outside the palette, a comment body that is empty, too long or carries control
	// characters. One sentinel rather than four because the handler answers all of them the same
	// way (422 with the reason), and four sentinels would be four chances to forget one.
	ErrInvalidSocialValue = errors.New("store: invalid value")
)

// The caps from core-scope §5 and §6, in one place because every backend must enforce them
// identically — the shared conformance suite holds the two to the same answers.
const (
	// MaxTagNameRunes bounds a tag name. Short on purpose: a label is read in a chip.
	MaxTagNameRunes = 64
	// MaxTagsPerNode bounds how many labels one file may carry, so a filter remains a filter.
	MaxTagsPerNode = 50
	// MaxTagsPerAccount bounds one account's personal tags.
	MaxTagsPerAccount = 200
	// MaxCommentRunes bounds a comment body (§6).
	MaxCommentRunes = 2000
	// MaxCommentBodyBytes bounds the stored bytes. The rune cap is the product rule; this exists
	// so a body of 2 000 four-byte runes cannot bypass a byte-oriented check somewhere downstream.
	MaxCommentBodyBytes = MaxCommentRunes * 4
)

// TagColors is the fixed palette (§5). A free-form colour would end up in a CSS value, and the
// first thing that happens to a user-chosen string that reaches CSS is a styling injection.
var TagColors = []string{
	"gray", "red", "orange", "yellow", "green", "teal", "blue", "indigo", "purple", "pink",
}

// Tag is a label. OwnerID nil marks a SYSTEM tag: admin-managed and visible to every account.
type Tag struct {
	ID      uuid.UUID
	OwnerID *uuid.UUID
	Name    string
	Color   string
	// CreatedAt is when the tag was defined, not when it was applied.
	CreatedAt time.Time
	CreatedBy *uuid.UUID
}

// IsSystem reports whether the tag is admin-managed.
func (t Tag) IsSystem() bool { return t.OwnerID == nil }

// ValidateTag normalises and validates a tag's name and colour.
//
// It is called by both backends rather than by the handler alone, for the reason the name
// validator lives in the storage layer: a rule enforced in one place and documented in another is
// a rule that eventually disagrees with itself.
func ValidateTag(t Tag) (Tag, error) {
	name := strings.TrimSpace(t.Name)
	if name == "" || utf8.RuneCountInString(name) > MaxTagNameRunes {
		return t, fmt.Errorf("%w: a tag name is 1-%d characters", ErrInvalidSocialValue, MaxTagNameRunes)
	}
	// Control characters and line breaks make a single-line chip unrenderable and are the shape a
	// log-injection payload takes. The same segment-validator philosophy as filenames.
	for _, r := range name {
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
			return t, fmt.Errorf("%w: a tag name cannot contain control characters", ErrInvalidSocialValue)
		}
	}
	color := strings.ToLower(strings.TrimSpace(t.Color))
	if color == "" {
		color = TagColors[0]
	}
	known := false
	for _, c := range TagColors {
		if c == color {
			known = true
			break
		}
	}
	if !known {
		return t, fmt.Errorf("%w: %q is not one of the tag colours (%s)",
			ErrInvalidSocialValue, t.Color, strings.Join(TagColors, ", "))
	}
	t.Name = name
	t.Color = color
	return t, nil
}

// Comment is one message in a node's discussion.
//
// Soft delete is deliberate (§6): a thread with a hole is unreadable, and "a comment was deleted"
// is a moderation fact that has to survive the removal. DeletedAt/DeletedBy are therefore data,
// not a tombstone marker to be cleaned up.
type Comment struct {
	ID uuid.UUID
	// Seq is the thread order: a monotonic per-database sequence, not a timestamp.
	//
	// "Oldest first" is what a discussion means, and created_at cannot express it. PostgreSQL's
	// now() is the transaction start time, so comments written in one transaction share it; the
	// in-memory clock is coarse enough (measured: no two consecutive time.Now() calls on this
	// platform differ) that several posts in a row share it too. Ordering by (created_at, id) then
	// breaks ties by a random UUID, which puts a reply above the message it answers. The sequence
	// is total by construction, which is also what makes the page cursor a single number.
	Seq       int64
	NodeID    uuid.UUID
	AuthorID  uuid.UUID
	ParentID  *uuid.UUID
	Body      string
	CreatedAt time.Time
	EditedAt  *time.Time
	DeletedAt *time.Time
	DeletedBy *uuid.UUID
}

// Deleted reports whether the comment has been removed (and is rendered as a tombstone).
func (c Comment) Deleted() bool { return c.DeletedAt != nil }

// ValidateCommentBody enforces the §6 content policy: plain text, a hard cap, no control
// characters.
//
// Markdown and HTML are not stripped, they are simply never interpreted: the body is handed to
// the client as text and the server never renders it. Refusing control characters is the part
// that matters here — a body made of NULs or bidirectional overrides is unreadable in a way the
// author can see and nobody else can, which is the same defect T-12 named for filenames.
func ValidateCommentBody(body string) (string, error) {
	if !utf8.ValidString(body) {
		return "", fmt.Errorf("%w: the comment is not valid UTF-8", ErrInvalidSocialValue)
	}
	if len(body) > MaxCommentBodyBytes {
		return "", fmt.Errorf("%w: a comment is at most %d characters",
			ErrInvalidSocialValue, MaxCommentRunes)
	}
	if utf8.RuneCountInString(body) > MaxCommentRunes {
		return "", fmt.Errorf("%w: a comment is at most %d characters",
			ErrInvalidSocialValue, MaxCommentRunes)
	}
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a comment cannot be empty", ErrInvalidSocialValue)
	}
	for _, r := range body {
		// Newline and tab are legitimate in a multi-line message; every other control character
		// is not, and the format characters (bidi overrides, zero-width joiners) render as
		// nothing and make one comment masquerade as another.
		if r == '\n' || r == '\r' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f || unicode.Is(unicode.Cf, r) {
			return "", fmt.Errorf("%w: a comment cannot contain control characters",
				ErrInvalidSocialValue)
		}
	}
	return body, nil
}

// CommentLimit is the rate limit applied to one post (§6). Limit <= 0 means unlimited.
//
// It is a parameter rather than a constant because the policy belongs to the deployment: the
// store enforces whatever the server hands it, and the conformance suite can then exercise both
// the limited and unlimited shapes against both backends.
type CommentLimit struct {
	Limit  int64
	Window time.Duration
}

// NodeSocial is the per-viewer annotation a listing needs for one node.
//
// It exists as a batch shape for the same reason Marker does: a directory of five hundred files
// must not issue five hundred round trips to render one screen. One query answers the whole page.
type NodeSocial struct {
	// Starred reports whether the VIEWER has starred this node. Per user, not per node — two
	// people sharing a folder star independently.
	Starred bool
	// Tags are the labels on this node that the viewer may see: system tags plus their own.
	Tags []Tag
	// SHA256 is the node's content address (core-scope §2). Empty for a directory, and empty when
	// the blob row is gone, which the caller renders as "unknown" rather than as zeroes.
	SHA256 []byte
}

// Favorite is one per-user star.
type Favorite struct {
	UserID    uuid.UUID
	NodeID    uuid.UUID
	CreatedAt time.Time
}

// FavoriteNode is a starred node together with the path it currently sits at.
type FavoriteNode struct {
	Node Node
	Path string
	// FavoritedAt is when the user starred it, which is the listing's sort key and, more
	// usefully, the thing a UI shows as "starred 3 days ago".
	//
	// It is exposed rather than kept private so a caller can build the next page cursor from the
	// row it was given. A cursor whose position the caller has to ask a second query for is a
	// cursor that can disagree with the listing it came from.
	FavoritedAt time.Time
}

// SocialTx is the social-metadata surface: favorites, tags and comments.
//
// It is one interface rather than three because the three arrived together, are reviewed together,
// and share the node-id addressing rule above. A backend that can store a favorite but not a tag
// would still have to implement the type, and splitting them would triple the conformance surface
// for no isolation benefit.
type SocialTx interface {
	// ---- favorites (§4) ----

	// SetFavorite stars a node for a user. Idempotent: starring twice is one row and no error,
	// because a client that retries after a dropped response must not have to care.
	SetFavorite(ctx context.Context, userID, nodeID uuid.UUID) error
	// UnsetFavorite removes a star. Idempotent for the same reason.
	UnsetFavorite(ctx context.Context, userID, nodeID uuid.UUID) error
	// IsFavorite reports whether this user has starred this node.
	IsFavorite(ctx context.Context, userID, nodeID uuid.UUID) (bool, error)
	// FavoritesAfter pages a user's starred nodes, newest first, keyset on (created_at, node_id).
	//
	// Trashed nodes are excluded by a join against live metadata: a star on a bin cannot be
	// reached from the listing, and showing it would offer a dead link. The row is not deleted —
	// restoring the node brings the star back, which is what a user expects from "I deleted it by
	// accident".
	FavoritesAfter(ctx context.Context, userID uuid.UUID, cursor string, limit int) ([]FavoriteNode, error)

	// ---- tags (§5) ----

	// CreateTag defines a tag. ownerID nil creates a SYSTEM tag, which is an administrator action:
	// the handler decides who may, this method only records what.
	CreateTag(ctx context.Context, t Tag) (Tag, error)
	// TagByID fetches one tag.
	TagByID(ctx context.Context, id uuid.UUID) (Tag, error)
	// TagsFor lists the tags visible to one account: the system tags plus that account's own,
	// ordered by name. An empty ownerID lists the system tags alone.
	TagsFor(ctx context.Context, ownerID uuid.UUID) ([]Tag, error)
	// UpdateTag renames and/or recolours a tag. The name is re-validated, and the case-fold
	// uniqueness rule applies exactly as it does on create.
	UpdateTag(ctx context.Context, id uuid.UUID, name, color string) (Tag, error)
	// DeleteTag removes a tag and, in one statement, every assignment of it. A dangling assignment
	// would be a filter that matches a tag nobody can see.
	DeleteTag(ctx context.Context, id uuid.UUID) error
	// AssignTag applies a tag to a node. Refuses a system tag to a non-admin caller only in the
	// sense that the handler does the deciding; the cap of MaxTagsPerNode is enforced here because
	// both backends must agree.
	AssignTag(ctx context.Context, nodeID, tagID uuid.UUID, by uuid.UUID) error
	// UnassignTag removes one assignment. Idempotent.
	UnassignTag(ctx context.Context, nodeID, tagID uuid.UUID) error
	// NodeTags lists the tags on one node that the given viewer may see.
	NodeTags(ctx context.Context, viewerID, nodeID uuid.UUID) ([]Tag, error)

	// ---- comments (§6) ----

	// CreateComment posts a comment, enforcing the cap and the rolling-window rate limit.
	//
	// limit is the deployment's policy; a Limit <= 0 disables the rate check. The count and the
	// insert are one atomic decision, for the same reason AllowShareUpload is: a separate count
	// followed by a separate insert lets two concurrent posts both observe "one under the limit".
	CreateComment(ctx context.Context, c Comment, limit CommentLimit) (Comment, error)
	// CommentByID fetches one comment including its tombstones, because the caller has to decide
	// whether the request is a moderation action and a deleted comment is still a row.
	CommentByID(ctx context.Context, id uuid.UUID) (Comment, error)
	// CommentsAfter pages a node's thread in posting order, strictly after the given seq.
	//
	// The cursor is the previous page's last Seq rather than the (time, id) pair the other listings
	// use. That is not an inconsistency: this ordering has a single monotonic key, which is exactly
	// what makes a keyset total, so composing one out of a timestamp and a random id would be
	// strictly worse. A seq of 0 starts at the beginning.
	CommentsAfter(ctx context.Context, nodeID uuid.UUID, afterSeq int64, limit int) ([]Comment, error)
	// UpdateComment replaces a body and stamps edited_at. A deleted comment cannot be edited.
	UpdateComment(ctx context.Context, id uuid.UUID, body string, at time.Time) (Comment, error)
	// DeleteComment soft-deletes a comment. The row and its body are kept so the thread stays
	// readable; the body is what the tombstone replaces in the rendering, not what is erased from
	// the database (an operator investigating abuse needs it, and §8 keeps it out of the chain
	// precisely so it stays in one place).
	DeleteComment(ctx context.Context, id uuid.UUID, by uuid.UUID, at time.Time) (Comment, error)
	// CountRecentComments reports how many comments an author posted inside the window, for a
	// caller that wants to refuse before it writes anything.
	CountRecentComments(ctx context.Context, authorID uuid.UUID, window time.Duration) (int64, error)
}

// Favorite and comment cursors.
//
// Opaque to clients per ADR-0012: the page position is an internal key, not an offset. Both are
// "<nanos>:<uuid>" — the same shape the trash cursor uses — because both orderings are
// (timestamp, id) and one parser is easier to keep correct than two.

// ParseSocialCursor decodes a "<unix-nanos>:<uuid>" cursor.
//
// A cursor this server did not issue is ErrInvalidCursor rather than a silent restart from the
// newest entry: a client holding a stale cursor would otherwise re-read page one forever while
// reporting progress.
func ParseSocialCursor(cursor string) (time.Time, uuid.UUID, error) {
	atStr, idStr, ok := strings.Cut(cursor, ":")
	if !ok {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: malformed cursor", ErrInvalidCursor)
	}
	nanos, err := strconv.ParseInt(atStr, 10, 64)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor timestamp", ErrInvalidCursor)
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("%w: cursor id", ErrInvalidCursor)
	}
	return time.Unix(0, nanos).UTC(), id, nil
}

// FormatSocialCursor encodes a (timestamp, id) page position.
//
// The id is part of the key rather than a nicety: two rows written in the same instant are
// otherwise indistinguishable, and a keyset that cannot break the tie either repeats a row at the
// page boundary or drops one — the failure ADR-0012 exists to prevent.
func FormatSocialCursor(at time.Time, id uuid.UUID) string {
	return fmt.Sprintf("%d:%s", at.UTC().UnixNano(), id)
}

// SocialReader is the optional batch annotation capability, the per-viewer sibling of MarkersFor.
//
// A backend that cannot answer it cheaply loses a decoration (the star, the tag chips, the
// checksum column) rather than failing the listing.
type SocialReader interface {
	// SocialFor returns per-viewer state for the given nodes.
	//
	// Nodes with nothing to report are omitted; a caller must use a two-value lookup. SHA256 is
	// reported for files whose content address is known and omitted otherwise, so "not stored
	// here" and "0x000…0" stay distinguishable.
	SocialFor(ctx context.Context, viewerID uuid.UUID, nodeIDs []uuid.UUID) (map[uuid.UUID]NodeSocial, error)
}

// TagCounter is the optional usage-count capability for the tag list.
//
// A separate optional interface rather than a field on TagsFor because the count is a decoration:
// a tag list without numbers is still a usable tag list, and a backend that cannot answer it
// cheaply should lose the numbers rather than the screen.
type TagCounter interface {
	// TagUsageCounts counts how many nodes carry each tag, as this viewer can see them.
	//
	// "As this viewer can see them" is load-bearing: a count that included nodes the caller may not
	// read would let them infer the existence of files they have no access to.
	TagUsageCounts(ctx context.Context, viewerID uuid.UUID, tagIDs []uuid.UUID) (map[uuid.UUID]int, error)
}

// UserLookup is the optional batch identity read the comment listing needs.
//
// A discussion whose author column says a UUID is unreadable to a person, and resolving one name
// per row would be an N+1 on the screen most likely to have many rows.
type UserLookup interface {
	// UsernamesFor returns display names for the given account ids. Unknown ids are omitted.
	UsernamesFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error)
}

// FavoriteCounter is the optional reverse count the search index needs.
//
// §7.2 names `favorite_count` as an indexed field. It is a count of OTHER people's stars, so it is
// not part of a viewer's own annotation and cannot be answered by SocialFor.
type FavoriteCounter interface {
	// FavoriteCount reports how many accounts have starred a node.
	FavoriteCount(ctx context.Context, nodeID uuid.UUID) (int, error)
}
