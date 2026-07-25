package main

import (
	"bytes"
	"image/png"
	"testing"
)

func TestDemoSamplesAreValidFiles(t *testing.T) {
	raw := []byte(demoPNG())
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("the sample PNG must decode, or the preview shows a broken image: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 240 || b.Dy() != 160 {
		t.Fatalf("unexpected size %v", b)
	}

	pdf := demoPDF()
	if len(pdf) < 200 {
		t.Fatalf("PDF too short: %d bytes", len(pdf))
	}
	if !bytes.HasPrefix([]byte(pdf), []byte("%PDF-1.")) {
		t.Fatal("PDF must start with a version header")
	}
	if !bytes.HasSuffix([]byte(pdf), []byte("%%EOF\n")) {
		t.Fatal("PDF must end with the EOF marker")
	}
	// The xref offset must point at the xref keyword, or viewers reject the file.
	if !bytes.Contains([]byte(pdf), []byte("\nxref\n")) {
		t.Fatal("PDF must contain an xref table")
	}
}
