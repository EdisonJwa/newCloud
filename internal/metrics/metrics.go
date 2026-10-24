// Package metrics is a small, dependency-free metrics registry in the Prometheus text
// exposition format (§11.1).
//
// Why not client_golang: this process needs counters and a gauge for queue depth, and the
// requirements name specific signals rather than a full instrumentation ecosystem. A
// hundred lines with tests beats a dependency that brings its own collector model, its own
// HTTP handler and its own opinions about naming. If the project later outgrows this, the
// exposition format is the compatible part and the registry can be swapped.
//
// What the requirement asks for, and where it lives:
//
//	请求错误率        newcloud_http_requests_total{status_class}
//	DAV 冲突率        newcloud_dav_conflicts_total
//	上传失败率        newcloud_uploads_total{result}
//	队列积压          newcloud_jobs{state}
//	GC 指标           newcloud_blob_gc_* (deleted/failed)
//	审计校验指标      newcloud_audit_events, newcloud_audit_chain_head_age_seconds
package metrics

import (
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Registry holds the counters and gauges.
//
// Counters are atomics rather than mutex-guarded maps because they are incremented on the
// request path: a lock there would serialise every request behind a metric, which is the
// classic way observability makes an outage worse.
type Registry struct {
	// httpRequests is keyed by method and status class, so error rate is a query rather
	// than a pre-computation. Bucketing statuses into classes keeps the cardinality fixed:
	// one series per route x status code would grow without bound as paths multiply.
	httpRequests sync.Map // map[string]*atomic.Uint64

	// davConflicts counts 409/412 responses on WebDAV, which is the signal that clients
	// are fighting over a file (or that lock handling is wrong).
	davConflicts atomic.Uint64
	// davRequests counts WebDAV requests, so the conflict rate has a denominator.
	davRequests atomic.Uint64

	// contentMissing counts responses for a file whose metadata exists and whose bytes the storage
	// layer could not produce. It is the only signal that distinguishes storage damage from a
	// missing file, and it should be zero forever: a non-zero rate means an operator has to look.
	contentMissing atomic.Uint64

	// uploads is keyed by result ("ok", "too_large", "failed", "aborted").
	uploads sync.Map // map[string]*atomic.Uint64

	// Thumbnail decode outcomes, keyed by result.
	thumbnails sync.Map // map[string]*atomic.Uint64

	// Audit chain gauges, refreshed by the worker.
	auditEvents      atomic.Int64
	auditHeadAgeNano atomic.Int64
	auditVerifyFail  atomic.Uint64
	auditVerifyOK    atomic.Uint64

	// Blob GC outcomes.
	blobsDeleted atomic.Uint64
	blobsGCError atomic.Uint64

	// Blob integrity scrub, pushed by the worker.
	//
	// corrupt and missing are the two numbers that matter: they mean storage is damaging data, or
	// that content the metadata references is gone. Nothing else in the system reads live blobs
	// back, so without these a corrupted file is discovered by a user.
	scrubChecked atomic.Int64
	scrubCorrupt atomic.Int64
	scrubMissing atomic.Int64

	// WAL archiving health, pushed by the worker from pg_stat_archiver.
	//
	// This is the signal that the disaster-recovery story has quietly stopped working. When
	// archive_command fails, PostgreSQL keeps running, keeps accepting writes, and keeps the
	// unarchived segments in pg_wal — so the deployment looks healthy while the RPO silently
	// degrades from minutes to "whatever is still on that disk". Measured (round 42): pg_wal grew
	// 49 -> 65 -> 81 MB across three rounds of writes with zero segments archived.
	walArchived atomic.Uint64
	walFailed   atomic.Uint64
	walPending  atomic.Int64

	// Authentication events that an operator must be able to alert on, keyed by kind.
	//
	// Break-glass use is the motivating case: §3.3 requires "使用后告警" — an alert when the local
	// administrator is used — and an audit row cannot be alerted on from a scrape. Without a
	// counter the requirement is satisfiable only by someone reading the log, which is not an
	// alert. The kind is a label rather than a separate counter per event so adding one is a
	// string at the call site, not a new field and a new exposition block.
	authEvents sync.Map // map[string]*atomic.Uint64

	// Queue depth, refreshed by the worker on each poll.
	jobsPending atomic.Int64
	jobsRunning atomic.Int64
	jobsFailed  atomic.Int64

	// Search-index health (core-scope §7.2).
	//
	// Two signals, because they answer the two questions an operator asks when content search
	// "stops working": is the index behind (lag), and is the engine failing (errors). Both are
	// absent on a deployment with no engine, which is correct — a gauge that reads zero because the
	// feature is off is a gauge that hides an outage on the deployment that has it.
	searchIndexLag  atomic.Int64
	searchIndexErrs atomic.Uint64

	startedAt time.Time
}

// New returns an empty registry.
func New() *Registry {
	return &Registry{startedAt: time.Now()}
}

// counter returns the counter for key, creating it on first use.
func counter(m *sync.Map, key string) *atomic.Uint64 {
	if v, ok := m.Load(key); ok {
		return v.(*atomic.Uint64)
	}
	actual, _ := m.LoadOrStore(key, new(atomic.Uint64))
	return actual.(*atomic.Uint64)
}

// allowedMethods is the fixed set of HTTP methods this server can observe, plus WebDAV's
// extensions. Anything outside it is recorded as "OTHER".
//
// The method is a LABEL on a counter, and a label fed straight from r.Method is unbounded
// cardinality: a client chooses its own method, so one garbage request per distinct spelling
// ("GET\u{0}", a lowercase "get", a 2 KiB custom verb) creates a series that lives for the
// lifetime of the process and is scraped forever. statusClass buckets the status for exactly
// this reason; the method needed the same treatment and did not have it.
var allowedMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "DELETE": true, "PATCH": true,
	"OPTIONS": true,
	// WebDAV (RFC 4918) and the REPORT/SEARCH extensions some clients send against
	// collections. These are the methods the /dav/ router actually dispatches on.
	"PROPFIND": true, "PROPPATCH": true, "MKCOL": true, "COPY": true, "MOVE": true,
	"LOCK": true, "UNLOCK": true, "REPORT": true, "SEARCH": true,
}

// normaliseMethod maps a raw request method onto the fixed label set.
//
// The comparison is exact rather than case-folding, deliberately: HTTP methods are
// case-sensitive, and folding "get" into "GET" would merge a malformed client into the
// series it is closest to instead of counting it as what it is — unusual traffic, visible
// under OTHER.
func normaliseMethod(method string) string {
	if allowedMethods[method] {
		return method
	}
	return "OTHER"
}

// ObserveRequest records one HTTP response.
func (r *Registry) ObserveRequest(method string, status int, dav bool) {
	counter(&r.httpRequests, normaliseMethod(method)+" "+statusClass(status)).Add(1)
	if dav {
		r.davRequests.Add(1)
		if status == http.StatusConflict || status == http.StatusPreconditionFailed {
			r.davConflicts.Add(1)
		}
	}
}

// ObserveUpload records an upload outcome.
func (r *Registry) ObserveUpload(result string) {
	counter(&r.uploads, result).Add(1)
}

// ObserveThumbnail records a thumbnail outcome.
func (r *Registry) ObserveThumbnail(result string) {
	counter(&r.thumbnails, result).Add(1)
}

// ObserveContentMissing records a file whose metadata exists but whose bytes do not.
func (r *Registry) ObserveContentMissing() {
	r.contentMissing.Add(1)
}

// ContentMissing reports how many times storage damage was served, for tests and for callers that
// want the number without scraping the exposition endpoint.
func (r *Registry) ContentMissing() uint64 { return r.contentMissing.Load() }

// SetBlobScrub reports the last integrity pass: how many blobs were read, how many disagreed with
// their address, and how many were referenced but absent.
func (r *Registry) SetBlobScrub(checked, corrupt, missing int) {
	r.scrubChecked.Store(int64(checked))
	r.scrubCorrupt.Store(int64(corrupt))
	r.scrubMissing.Store(int64(missing))
}

// SetWALArchive reports the WAL archiver's state: segments archived, segments failed, and how
// many are waiting for a successful archive. The third is the one that grows toward a full disk.
func (r *Registry) SetWALArchive(archived, failed uint64, pending int64) {
	r.walArchived.Store(archived)
	r.walFailed.Store(failed)
	r.walPending.Store(pending)
}

// ObserveAuth records a security-relevant authentication event, keyed by kind.
//
// The kinds are deliberately a small closed set — "breakglass_ok", "breakglass_denied",
// "login_denied" — because this is an alerting signal, not an analytics feed: a label whose values
// proliferate makes every alert rule that matches on it fragile.
func (r *Registry) ObserveAuth(kind string) {
	counter(&r.authEvents, kind).Add(1)
}

// SetAudit reports the chain's length and the age of its head.
func (r *Registry) SetAudit(events int64, headAge time.Duration) {
	r.auditEvents.Store(events)
	r.auditHeadAgeNano.Store(int64(headAge))
}

// ObserveAuditVerify records an anchor verification outcome.
func (r *Registry) ObserveAuditVerify(ok bool) {
	if ok {
		r.auditVerifyOK.Add(1)
		return
	}
	r.auditVerifyFail.Add(1)
}

// ObserveBlobGC records the blob collector's outcome.
func (r *Registry) ObserveBlobGC(deleted int, failed bool) {
	r.blobsDeleted.Add(uint64(deleted))
	if failed {
		r.blobsGCError.Add(1)
	}
}

// SetJobs reports the queue depth.
func (r *Registry) SetJobs(pending, running, failed int) {
	r.jobsPending.Store(int64(pending))
	r.jobsRunning.Store(int64(running))
	r.jobsFailed.Store(int64(failed))
}

// SetSearchIndexLag reports how far behind the content index is.
//
// Seconds since the oldest still-pending index job's due time. Zero means the index is caught up;
// a number that keeps growing means the queue is not draining and searches are running against a
// stale cache. On a deployment with no engine this is never set, and the alert therefore never
// fires — which is why the alert rule pairs it with the error counter rather than firing on lag
// alone.
func (r *Registry) SetSearchIndexLag(d time.Duration) {
	if d < 0 {
		d = 0
	}
	r.searchIndexLag.Store(int64(d.Seconds()))
}

// ObserveSearchIndexError records one failure to write to or read from the index.
//
// Both directions count: a failed index write is why the index is stale, and a failed query is why
// a search answered `degraded: true`. An operator alerting on "content search is degraded" wants
// both in one series.
func (r *Registry) ObserveSearchIndexError() {
	r.searchIndexErrs.Add(1)
}

// statusClass buckets a status code.
//
// 2xx/3xx/4xx/5xx rather than the exact code: the useful question is "what fraction of
// requests are failing", and exact-code series per route is how a metrics endpoint becomes
// a memory leak.
func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	case status >= 200:
		return "2xx"
	default:
		return "1xx"
	}
}

// Handler serves the Prometheus text format.
//
// The format is written by hand, which is why the labels are quoted and escaped: a label
// value containing a quote or a newline would otherwise produce a document that scrapers
// silently reject, and a metrics endpoint that is subtly malformed is worse than none.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		r.write(w)
	})
}

// write emits the exposition document.
func (r *Registry) write(w http.ResponseWriter) {
	// HTTP requests, sorted for a stable document: an unordered dump makes a scrape diff
	// useless and hides real changes among reshuffled lines.
	fmt.Fprintln(w, "# HELP newcloud_http_requests_total HTTP responses by method and status class.")
	fmt.Fprintln(w, "# TYPE newcloud_http_requests_total counter")
	var httpKeys []string
	r.httpRequests.Range(func(k, _ any) bool {
		httpKeys = append(httpKeys, k.(string))
		return true
	})
	sort.Strings(httpKeys)
	for _, k := range httpKeys {
		method, class := splitKey(k)
		v, _ := r.httpRequests.Load(k)
		fmt.Fprintf(w, "newcloud_http_requests_total{method=%q,status_class=%q} %d\n",
			method, class, v.(*atomic.Uint64).Load())
	}

	fmt.Fprintln(w, "# HELP newcloud_dav_requests_total WebDAV requests.")
	fmt.Fprintln(w, "# TYPE newcloud_dav_requests_total counter")
	fmt.Fprintf(w, "newcloud_dav_requests_total %d\n", r.davRequests.Load())

	fmt.Fprintln(w, "# HELP newcloud_dav_conflicts_total WebDAV 409/412 responses (lock contention or a stale etag).")
	fmt.Fprintln(w, "# TYPE newcloud_dav_conflicts_total counter")
	fmt.Fprintf(w, "newcloud_dav_conflicts_total %d\n", r.davConflicts.Load())
	// HELP/TYPE are emitted for content_missing too: the exposition format has no default
	// metric type, and a scrape of an untyped counter is rejected or guessed by collectors
	// depending on which one is reading — an alert built on it then behaves differently by
	// scraper version.
	fmt.Fprintln(w, "# HELP newcloud_content_missing_total Responses for a file whose metadata exists but whose stored bytes could not be produced.")
	fmt.Fprintln(w, "# TYPE newcloud_content_missing_total counter")
	fmt.Fprintf(w, "newcloud_content_missing_total %d\n", r.contentMissing.Load())

	writeCounterMap(w, "newcloud_uploads_total", "Upload outcomes.", &r.uploads)
	writeCounterMap(w, "newcloud_thumbnails_total", "Thumbnail decode outcomes.", &r.thumbnails)

	fmt.Fprintln(w, "# HELP newcloud_jobs Queue depth by state.")
	fmt.Fprintln(w, "# TYPE newcloud_jobs gauge")
	fmt.Fprintf(w, "newcloud_jobs{state=\"pending\"} %d\n", r.jobsPending.Load())
	fmt.Fprintf(w, "newcloud_jobs{state=\"running\"} %d\n", r.jobsRunning.Load())
	fmt.Fprintf(w, "newcloud_jobs{state=\"failed\"} %d\n", r.jobsFailed.Load())

	// Content-search health (core-scope §7.2). Both families are always emitted, so an alert rule
	// naming them is valid on every deployment and reads zero on one without an engine — which is
	// the honest value ("there is no index to be behind") rather than a missing series that a scrape
	// would treat as "no data" forever.
	fmt.Fprintln(w, "# HELP newcloud_search_index_lag_seconds How far behind the content index is.")
	fmt.Fprintln(w, "# TYPE newcloud_search_index_lag_seconds gauge")
	fmt.Fprintf(w, "newcloud_search_index_lag_seconds %d\n", r.searchIndexLag.Load())
	fmt.Fprintln(w, "# HELP newcloud_search_index_errors_total Failed index reads and writes.")
	fmt.Fprintln(w, "# TYPE newcloud_search_index_errors_total counter")
	fmt.Fprintf(w, "newcloud_search_index_errors_total %d\n", r.searchIndexErrs.Load())

	fmt.Fprintln(w, "# HELP newcloud_blob_scrub_corrupt Blobs whose bytes do not match their address.")
	fmt.Fprintln(w, "# TYPE newcloud_blob_scrub_corrupt gauge")
	fmt.Fprintf(w, "newcloud_blob_scrub_corrupt %d\n", r.scrubCorrupt.Load())
	fmt.Fprintln(w, "# HELP newcloud_blob_scrub_missing Blobs referenced by metadata but absent from storage.")
	fmt.Fprintln(w, "# TYPE newcloud_blob_scrub_missing gauge")
	fmt.Fprintf(w, "newcloud_blob_scrub_missing %d\n", r.scrubMissing.Load())
	fmt.Fprintln(w, "# HELP newcloud_blob_scrub_checked Blobs verified in the last integrity pass.")
	fmt.Fprintln(w, "# TYPE newcloud_blob_scrub_checked gauge")
	fmt.Fprintf(w, "newcloud_blob_scrub_checked %d\n", r.scrubChecked.Load())

	fmt.Fprintln(w, "# HELP newcloud_wal_archived_total WAL segments archived successfully.")
	fmt.Fprintln(w, "# TYPE newcloud_wal_archived_total counter")
	fmt.Fprintf(w, "newcloud_wal_archived_total %d\n", r.walArchived.Load())
	fmt.Fprintln(w, "# HELP newcloud_wal_archive_failed_total WAL archive attempts that failed.")
	fmt.Fprintln(w, "# TYPE newcloud_wal_archive_failed_total counter")
	fmt.Fprintf(w, "newcloud_wal_archive_failed_total %d\n", r.walFailed.Load())
	fmt.Fprintln(w, "# HELP newcloud_wal_segments_pending WAL segments waiting to be archived.")
	fmt.Fprintln(w, "# TYPE newcloud_wal_segments_pending gauge")
	fmt.Fprintf(w, "newcloud_wal_segments_pending %d\n", r.walPending.Load())

	fmt.Fprintln(w, "# HELP newcloud_auth_events_total Authentication events by kind.")
	fmt.Fprintln(w, "# TYPE newcloud_auth_events_total counter")
	r.authEvents.Range(func(k, v any) bool {
		fmt.Fprintf(w, "newcloud_auth_events_total{kind=%q} %d\n", k.(string), v.(*atomic.Uint64).Load())
		return true
	})

	fmt.Fprintln(w, "# HELP newcloud_audit_events Events in the audit chain.")
	fmt.Fprintln(w, "# TYPE newcloud_audit_events gauge")
	fmt.Fprintf(w, "newcloud_audit_events %d\n", r.auditEvents.Load())

	fmt.Fprintln(w, "# HELP newcloud_audit_chain_head_age_seconds Age of the newest audit event.")
	fmt.Fprintln(w, "# TYPE newcloud_audit_chain_head_age_seconds gauge")
	fmt.Fprintf(w, "newcloud_audit_chain_head_age_seconds %.3f\n",
		time.Duration(r.auditHeadAgeNano.Load()).Seconds())

	fmt.Fprintln(w, "# HELP newcloud_audit_anchor_verify_total Anchor verifications by outcome.")
	fmt.Fprintln(w, "# TYPE newcloud_audit_anchor_verify_total counter")
	fmt.Fprintf(w, "newcloud_audit_anchor_verify_total{result=\"ok\"} %d\n", r.auditVerifyOK.Load())
	fmt.Fprintf(w, "newcloud_audit_anchor_verify_total{result=\"failed\"} %d\n", r.auditVerifyFail.Load())

	fmt.Fprintln(w, "# HELP newcloud_blob_gc_deleted_total Blobs removed by the collector.")
	fmt.Fprintln(w, "# TYPE newcloud_blob_gc_deleted_total counter")
	fmt.Fprintf(w, "newcloud_blob_gc_deleted_total %d\n", r.blobsDeleted.Load())

	fmt.Fprintln(w, "# HELP newcloud_blob_gc_errors_total Blob collection runs that failed.")
	fmt.Fprintln(w, "# TYPE newcloud_blob_gc_errors_total counter")
	fmt.Fprintf(w, "newcloud_blob_gc_errors_total %d\n", r.blobsGCError.Load())

	fmt.Fprintln(w, "# HELP newcloud_uptime_seconds Seconds since this process started.")
	fmt.Fprintln(w, "# TYPE newcloud_uptime_seconds gauge")
	fmt.Fprintf(w, "newcloud_uptime_seconds %.0f\n", time.Since(r.startedAt).Seconds())
}

// writeCounterMap emits one family from a keyed counter map.
func writeCounterMap(w http.ResponseWriter, name, help string, m *sync.Map) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s counter\n", name)

	var keys []string
	m.Range(func(k, _ any) bool {
		keys = append(keys, k.(string))
		return true
	})
	sort.Strings(keys)
	for _, k := range keys {
		v, _ := m.Load(k)
		fmt.Fprintf(w, "%s{result=%q} %d\n", name, k, v.(*atomic.Uint64).Load())
	}
}

// splitKey splits a "method class" key.
func splitKey(k string) (string, string) {
	for i := len(k) - 1; i >= 0; i-- {
		if k[i] == ' ' {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// Snapshot is a point-in-time copy, for tests and the admin view.
type Snapshot struct {
	HTTPRequests   map[string]uint64
	DAVRequests    uint64
	DAVConflicts   uint64
	ContentMissing uint64
	Uploads        map[string]uint64
	QueuePending   int64
	QueueRunning   int64
	QueueFailed    int64
}

// Snapshot returns the current values.
func (r *Registry) Snapshot() Snapshot {
	s := Snapshot{
		HTTPRequests:   map[string]uint64{},
		Uploads:        map[string]uint64{},
		DAVRequests:    r.davRequests.Load(),
		DAVConflicts:   r.davConflicts.Load(),
		ContentMissing: r.contentMissing.Load(),
		QueuePending:   r.jobsPending.Load(),
		QueueRunning:   r.jobsRunning.Load(),
		QueueFailed:    r.jobsFailed.Load(),
	}
	r.httpRequests.Range(func(k, v any) bool {
		s.HTTPRequests[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	r.uploads.Range(func(k, v any) bool {
		s.Uploads[k.(string)] = v.(*atomic.Uint64).Load()
		return true
	})
	return s
}
