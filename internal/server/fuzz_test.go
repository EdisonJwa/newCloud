package server

// Fuzzing the Content-Range parser.
//
// This parser decides where an upload's bytes are written. A lenient one accepts a range that does not
// describe the body and writes at an offset nobody asked for; an arithmetic bug lets a chunk claim a
// span larger than the file. Both are data corruption, so the invariants below are checked on every
// input the parser accepts, not just on the examples in the unit tests.

import (
	"strings"
	"testing"
)

func FuzzParseContentRange(f *testing.F) {
	for _, seed := range []string{
		"bytes 0-1023/2048", "bytes 0-0/1", "bytes 1023-1023/1024", "bytes 0-1023/*",
		"bytes 5-4/10", "bytes -1-4/10", "bytes 0-4/-1", "bytes 0-4/0", "bytes 0-4",
		"bytes 0/4", "bytes 0-4/4", "bytes 99999999999999999999-1/2", " bytes 0-1/2 ",
		"bytes  0-1/2", "BYTES 0-1/2", "bytes 0-1/2/3", "bytes 0--1/2", "",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, header string) {
		start, end, total, err := parseContentRange(header)
		if err != nil {
			return
		}

		if start < 0 {
			t.Fatalf("accepted %q with a negative start %d", header, start)
		}
		if end < start {
			t.Fatalf("accepted %q with end %d before start %d", header, end, start)
		}
		if total != 0 {
			// A real total: the span must fit inside it. end == total would describe one byte past
			// the end of the file.
			if end >= total {
				t.Fatalf("accepted %q whose span ends at %d but the total is %d", header, end, total)
			}
		} else {
			// total == 0 is the wildcard form; it must be spelled "*", not an unparseable number.
			if !strings.HasSuffix(strings.TrimSpace(header), "/*") {
				t.Fatalf("accepted %q with total 0 but no wildcard total", header)
			}
		}
	})
}
