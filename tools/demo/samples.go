package main

// Sample content for the demo.
//
// The files are generated rather than checked in as binaries: a PNG and a PDF small enough
// to be readable are twenty lines of code each, and a demo that ships real media would
// carry a megabyte of assets nobody can diff.
//
// They must be valid files, not placeholders. A "PDF" that is not a PDF makes the preview
// look broken rather than the sample data look fake, and the visitor blames the product.

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"hash/crc32"
)

const demoReadme = `newcloud 演示实例
=================

这个实例里的一切都是真的：真实的 PostgreSQL、真实的 WebDAV、
真实的哈希链审计日志。只有身份提供方是本地模拟的，因为它通常
需要一台单独的 Pocket ID 和 passkey。

可以试的：

  * 点任意文件预览 —— 图片、PDF、文本都走同一套预览通道
  * 右键或点 Share 创建链接，看密码 / 次数 / 有效期
  * 拖文件到窗口任意位置上传
  * 在 Trash 里删除再恢复
  * App passwords 里生成一个 WebDAV 凭证

存储方式
--------

文件内容按 SHA-256 存在本地磁盘上（内容寻址），目录树只存在于
PostgreSQL。这意味着同样的字节只存一份，但也意味着 ` + "`ls`" + ` 看不到原文件名。
另一种布局（tree）会把目录结构直接映射到磁盘，代价是没有去重。
`

const demoMarkdown = `# 季度报告

## 概要

| 指标 | 上季度 | 本季度 | 变化 |
|---|---|---|---|
| 新增用户 | 1,204 | 1,891 | +57% |
| 存储用量 | 12 TB | 18 TB | +50% |
| 平均上传大小 | 4.2 MB | 5.8 MB | +38% |

## 结论

存储增长快于用户增长，说明单用户用量在上升。下一步应该关注
大文件的分片上传，而不是继续优化小文件路径。

## 代码示例

` + "```go" + `
func (s *Store) Put(ctx context.Context, n Node, r io.Reader, size int64) (Result, error) {
    // 先写内容，再写元数据：内容失败则无痕迹，元数据失败则留下孤儿 blob，
    // 后者由回收任务清理，前者不可能出现。
    return s.publish(ctx, n, r, size)
}
` + "```" + `
`

const demoCSV = `date,user,action,bytes,duration_ms
2026-10-21,alice,upload,1048576,142
2026-10-21,alice,download,1048576,88
2026-10-21,bob,upload,52428800,2210
2026-10-21,bob,share.create,0,12
2026-10-21,carol,download,52428800,1904
2026-10-20,alice,login,0,340
2026-10-20,bob,login,0,297
`

// demoPNG builds a small, valid PNG: a 240x160 gradient.
//
// A real encoder is not needed. The format is a signature followed by chunks, each with a
// CRC, and writing those by hand keeps the dependency list at zero.
func demoPNG() string {
	const (
		w = 240
		h = 160
	)

	// Raw scanlines: one filter byte (0 = none) then RGB triples.
	raw := make([]byte, 0, h*(1+w*3))
	for y := 0; y < h; y++ {
		raw = append(raw, 0)
		for x := 0; x < w; x++ {
			// A diagonal blend between the accent blue and a violet, so the thumbnail is
			// recognisable at 30 pixels.
			t := float64(x+y) / float64(w+h)
			raw = append(raw,
				byte(47+(123-47)*t),
				byte(109+(92-109)*t),
				byte(246+(255-246)*t),
			)
		}
	}

	var out bytes.Buffer
	out.Write([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'})

	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8] = 8  // bit depth
	ihdr[9] = 2  // colour type: truecolour
	ihdr[10] = 0 // compression
	ihdr[11] = 0 // filter
	ihdr[12] = 0 // interlace
	writeChunk(&out, "IHDR", ihdr)

	var compressed bytes.Buffer
	zw := zlib.NewWriter(&compressed)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	writeChunk(&out, "IDAT", compressed.Bytes())

	writeChunk(&out, "IEND", nil)
	return out.String()
}

// writeChunk appends one PNG chunk.
func writeChunk(w *bytes.Buffer, kind string, data []byte) {
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(data)))
	w.Write(length[:])
	w.WriteString(kind)
	w.Write(data)

	crc := crc32.NewIEEE()
	crc.Write([]byte(kind))
	crc.Write(data)
	var sum [4]byte
	binary.BigEndian.PutUint32(sum[:], crc.Sum32())
	w.Write(sum[:])
}

// demoPDF builds a minimal but valid single-page PDF.
//
// The cross-reference table needs correct byte offsets, so the body is assembled while the
// offsets are recorded rather than computed afterwards. A wrong xref makes most viewers
// refuse the file, which is exactly the "looks broken" outcome this avoids.
func demoPDF() string {
	var buf bytes.Buffer

	objects := make([]string, 0, 4)
	objects = append(objects,
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] "+
			"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
	)

	content := `BT /F1 28 Tf 72 720 Td (Purchase Contract 2026) Tj ET
BT /F1 12 Tf 72 680 Td (This is a demonstration document.) Tj ET
BT /F1 12 Tf 72 660 Td (It is a real PDF: the preview renders it.) Tj ET`
	objects = append(objects, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	objects = append(objects, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	buf.WriteString("%PDF-1.4\n%\xE2\xE3\xCF\xD3\n")

	offsets := make([]int, len(objects)+1)
	for i, obj := range objects {
		offsets[i+1] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}

	xrefAt := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", len(objects)+1)
	buf.WriteString("0000000000 65535 f \n")
	for i := 1; i <= len(objects); i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n",
		len(objects)+1, xrefAt)

	return buf.String()
}
