package main

// A minimal multipart encoder.
//
// The demo uploads four small text files; pulling in mime/multipart would work, but its
// writer wants an io.Writer and the caller here builds the body in memory anyway. Ten
// lines keep the seeding code free of error handling that cannot fail.

import (
	"fmt"
	"mime/multipart"
	"net/textproto"
	"strings"
)

type multipartWriter struct {
	body     *strings.Builder
	mw       *multipart.Writer
	boundary string
}

// newMultipart starts a body.
func newMultipart(body *strings.Builder) *multipartWriter {
	mw := multipart.NewWriter(body)
	return &multipartWriter{body: body, mw: mw, boundary: mw.Boundary()}
}

func (w *multipartWriter) writeField(name, value string) error {
	return w.mw.WriteField(name, value)
}

func (w *multipartWriter) writeFile(field, filename, content string) error {
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition",
		fmt.Sprintf(`form-data; name=%q; filename=%q`, field, filename))
	h.Set("Content-Type", "application/octet-stream")

	part, err := w.mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = part.Write([]byte(content))
	return err
}

func (w *multipartWriter) close() error { return w.mw.Close() }

func (w *multipartWriter) contentType() string {
	return "multipart/form-data; boundary=" + w.boundary
}
