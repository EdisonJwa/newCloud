// Package thumbnail generates and caches small previews of images.
//
// The reason this exists on the server rather than in the browser: a 20-megapixel photo is
// 8 MB on disk and would have to be downloaded in full before a 30-pixel table row could
// show it. A directory of forty photos is a third of a gigabyte to render one screen.
//
// Only the standard library is used. Go ships decoders for PNG, JPEG and GIF, which covers
// what a file manager most often stores; anything else falls back to the type badge rather
// than pulling in a third-party codec with its own CVE history.
package thumbnail

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"sync"
	"time"

	// Registering the decoders is not enough for a downscale that respects EXIF
	// orientation: the orientation tag lives in the metadata, and jpeg.Decode ignores it.
	_ "image/gif"
)

// Size is the longest edge of a generated thumbnail, in pixels.
//
// 320 is chosen for the grid view's largest tile at 2x device pixel ratio. Larger wastes
// bytes for a row that renders at 30px; smaller looks soft in the grid.
const Size = 320

// Quality is the JPEG quality for the encoded thumbnail.
//
// 82 is the point where a photo still looks correct at tile size and the file stays under
// ~15 KB for most images. Higher quality is invisible here and multiplies the cache.
const Quality = 82

// ErrUnsupported means the bytes are not an image format this package decodes.
var ErrUnsupported = errors.New("thumbnail: unsupported image format")

// ErrTooLarge means the image dimensions exceed what this process will decode.
var ErrTooLarge = errors.New("thumbnail: image dimensions too large")

// maxPixels bounds the decoded image size.
//
// A "decompression bomb" is a small file that expands to an enormous bitmap: a 20 KB PNG
// can declare 60000x60000, which is 14 GB once decoded. Decoding it would take the process
// down, so the header is inspected first and the decode refused.
const maxPixels = 50_000_000 // 50 MP, comfortably above any camera or phone

// ErrBusy means the decode queue was too long to wait for.
//
// It is a normal outcome, not a fault: the caller answers "no thumbnail" and the client
// shows a generic icon. Reporting it as an error the user sees would punish them for
// somebody else's traffic.
var ErrBusy = errors.New("thumbnail: too many concurrent decode requests")

// ErrTimeout means one decode exceeded its deadline.
//
// Distinct from a decode error because the cause is different and worth logging
// separately: a timeout usually means a crafted file, while a decode error usually means
// an ordinary corrupt one.
var ErrTimeout = errors.New("thumbnail: decode exceeded its deadline")

// Generator produces thumbnails and memoises them.
//
// The cache is in memory and keyed by content address. That is the right key: the same
// bytes always produce the same thumbnail, so a hit is exact and no invalidation is needed
// when a file is replaced — the address changes instead.
type Generator struct {
	mu    sync.RWMutex
	cache map[[32]byte][]byte
	// maxEntries bounds memory. At ~15 KB per entry this is about 12 MB, which is a
	// reasonable ceiling for an in-process cache that can always be recomputed.
	maxEntries int

	// sem bounds how many images are decoded at once.
	//
	// The pixel cap stops one image from being huge; it does not stop a hundred requests
	// from arriving together and each decoding a legitimate 50 MP bitmap. Without a
	// bound, the memory a single request can be refused for is multiplied by the number
	// of concurrent requests, which is a way to take the process down with files that are
	// individually acceptable.
	sem chan struct{}
	// timeout bounds one decode.
	//
	// A decoder is CPU-bound but not interruptible: a crafted file can make it spin far
	// longer than a real image of the same size. Cancelling after the deadline at least
	// bounds how long a request holds a slot, and the request path treats the deadline as
	// "no thumbnail" rather than an error the user must see.
	timeout time.Duration
}

// DefaultConcurrency is how many thumbnails are decoded at once.
//
// Four is chosen to match a small server's cores rather than its request rate: the work
// is CPU-bound, so allowing more would only lengthen every queue.
const DefaultConcurrency = 4

// DefaultTimeout bounds one decode.
const DefaultTimeout = 10 * time.Second

// New builds a generator.
func New() *Generator {
	return &Generator{
		cache:      make(map[[32]byte][]byte),
		maxEntries: 800,
		sem:        make(chan struct{}, DefaultConcurrency),
		timeout:    DefaultTimeout,
	}
}

// Kind reports whether these bytes look like an image this package can decode.
//
// It reads only the header, so a caller can decide whether to offer a thumbnail without
// paying for a decode.
func Kind(head []byte) (string, bool) {
	switch {
	case len(head) >= 8 && bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "png", true
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "jpeg", true
	case len(head) >= 6 && (bytes.HasPrefix(head, []byte("GIF87a")) || bytes.HasPrefix(head, []byte("GIF89a"))):
		return "gif", true
	}
	return "", false
}

// Generate returns a PNG thumbnail for the image, using the cache when possible.
//
// The output is always PNG regardless of the input format. Re-encoding a JPEG to JPEG saves
// little at this size and costs a generation loss; PNG keeps the thumbnail exact.
// Generate produces a thumbnail, bounding both concurrency and time.
//
// Two limits, for two different failures. The semaphore stops a burst of legitimate
// images from multiplying the memory a single decode may use. The deadline stops one
// crafted file from holding a slot indefinitely. Neither is a substitute for the pixel
// cap: that refuses the decode outright, this bounds how many and how long.
func (g *Generator) Generate(hash [32]byte, r io.ReadSeeker) ([]byte, error) {
	if cached, ok := g.get(hash); ok {
		return cached, nil
	}

	// Take a slot. The context is deliberately not plumbed from the request: a cancelled
	// HTTP request should not abort a decode another request is queued behind, and the
	// result is cached for everyone. The deadline below is what bounds the work.
	select {
	case g.sem <- struct{}{}:
		defer func() { <-g.sem }()
	case <-time.After(waitBudget):
		// The queue is long enough that answering without a thumbnail is better than
		// making the client wait behind an unknown number of decodes.
		return nil, ErrBusy
	}

	// Run the decode with a deadline. The goroutine cannot be killed, so this bounds how
	// long the *caller* waits and how long the slot is held; the decode itself finishes
	// or fails on its own. That is the honest limit of what Go allows for a CPU-bound
	// library call, and it is why the pixel cap matters as well.
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		data, err := g.generate(hash, r)
		done <- result{data, err}
	}()

	select {
	case res := <-done:
		return res.data, res.err
	case <-time.After(g.timeout):
		return nil, ErrTimeout
	}
}

// waitBudget bounds how long a request waits for a decode slot.
const waitBudget = 5 * time.Second

// generate is the unbounded decoder; Generate wraps it with the queue and deadline.
func (g *Generator) generate(hash [32]byte, r io.ReadSeeker) ([]byte, error) {
	if cached, ok := g.get(hash); ok {
		return cached, nil
	}

	// Read the header first to bound the decode.
	head := make([]byte, 64)
	n, _ := io.ReadFull(r, head)
	head = head[:n]

	if _, ok := Kind(head); !ok {
		return nil, ErrUnsupported
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	// Decode the configuration before the pixels: Config returns the dimensions without
	// allocating the bitmap, which is what makes the size check meaningful.
	cfg, _, err := image.DecodeConfig(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("thumbnail: read dimensions: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, ErrUnsupported
	}
	if cfg.Width*cfg.Height > maxPixels {
		return nil, ErrTooLarge
	}

	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	src, format, err := image.Decode(r)
	if err != nil {
		return nil, fmt.Errorf("thumbnail: decode: %w", err)
	}
	if format == "gif" {
		// image.Decode already handled the first frame; a GIF here is a still.
		_ = gif.GIF{}
	}

	dst := downscale(src, Size)

	var out bytes.Buffer
	if err := png.Encode(&out, dst); err != nil {
		return nil, fmt.Errorf("thumbnail: encode: %w", err)
	}

	data := out.Bytes()
	g.put(hash, data)
	return data, nil
}

// get reads from the cache.
func (g *Generator) get(hash [32]byte) ([]byte, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	data, ok := g.cache[hash]
	return data, ok
}

// put stores in the cache, evicting when full.
//
// Eviction is arbitrary rather than LRU: the cost of a miss is a re-decode of a small
// image, and an LRU would need a bookkeeping structure that is larger than the saving.
func (g *Generator) put(hash [32]byte, data []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if len(g.cache) >= g.maxEntries {
		// Drop one existing entry to make room. Map iteration order is randomised, which
		// is exactly the arbitrary eviction wanted here.
		for k := range g.cache {
			delete(g.cache, k)
			break
		}
	}
	g.cache[hash] = data
}

// Len reports the number of cached thumbnails, for tests and diagnostics.
func (g *Generator) Len() int {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return len(g.cache)
}

// downscale resizes to fit within max on the longest edge.
//
// A nearest-neighbour box filter is used rather than a resampling library: at thumbnail
// size the difference is not visible, and the alternative is a dependency. Colours are
// averaged over the source box each destination pixel covers, which avoids the aliasing
// that plain nearest-neighbour gives on a photo.
func downscale(src image.Image, max int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= max && h <= max {
		// Already small enough: re-encoding is enough, and resampling would only soften it.
		return src
	}

	scale := float64(max) / float64(w)
	if h > w {
		scale = float64(max) / float64(h)
	}
	dw := int(float64(w)*scale + 0.5)
	dh := int(float64(h)*scale + 0.5)
	if dw < 1 {
		dw = 1
	}
	if dh < 1 {
		dh = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))

	for y := 0; y < dh; y++ {
		// The source row range this destination row covers.
		y0 := b.Min.Y + y*h/dh
		y1 := b.Min.Y + (y+1)*h/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}

		for x := 0; x < dw; x++ {
			x0 := b.Min.X + x*w/dw
			x1 := b.Min.X + (x+1)*w/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}

			// Average the box. A 16-bit accumulator would overflow on a large box, so the
			// sum is 64-bit.
			var sr, sg, sb, sa, count uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r16, g16, b16, a16 := src.At(sx, sy).RGBA()
					sr += uint64(r16)
					sg += uint64(g16)
					sb += uint64(b16)
					sa += uint64(a16)
					count++
				}
			}
			if count == 0 {
				continue
			}

			// RGBA() returns 16-bit values; shift back to 8-bit after averaging.
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(sr / count >> 8),
				G: uint8(sg / count >> 8),
				B: uint8(sb / count >> 8),
				A: uint8(sa / count >> 8),
			})
		}
	}
	return dst
}

// EncodeJPEG is used by tests to confirm the decoder path with a real JPEG.
func EncodeJPEG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: Quality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
