// Image bytes and ordinary files travel in bounded chunks. Paths belong to
// the local machine only; the wire manifest carries leaf names and lengths.
package clipshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	ChunkSize = 256 << 10
	MaxImage  = 32 << 20
	MaxPixels = 32 << 20
	MaxFiles  = 128
	MaxTotal  = 1 << 30
)

type Content struct {
	cleanup func()
	PNG     []byte
	Files   []string
}

func (c Content) Empty() bool { return len(c.PNG) == 0 && len(c.Files) == 0 }

type RichBoard interface {
	Board
	ReadRich(context.Context) (Content, error)
	WriteRich(Content) error
}

type Entry struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
}
type Manifest struct {
	Kind    string  `json:"kind"`
	Entries []Entry `json:"entries"`
}
type Part struct {
	Manifest *Manifest `json:"manifest,omitempty"`
	Offset   int64     `json:"offset,omitempty"`
	Bytes    []byte    `json:"bytes,omitempty"`
}

func safeName(s string) bool {
	return s != "" && utf8.ValidString(s) && len(s) <= 255 && filepath.IsLocal(s) && s != "." &&
		!strings.ContainsAny(s, "/\\:\x00") && !strings.HasSuffix(s, ".") && !strings.HasSuffix(s, " ")
}

func (m Manifest) size() (int64, error) {
	if m.Kind == "png" {
		if len(m.Entries) != 1 || m.Entries[0].Size <= 0 || m.Entries[0].Size > MaxImage {
			return 0, errors.New("剪贴板图片超出大小限制")
		}
	} else if m.Kind != "files" || len(m.Entries) == 0 || len(m.Entries) > MaxFiles {
		return 0, errors.New("无效的剪贴板文件列表")
	}
	var total int64
	for _, e := range m.Entries {
		if !safeName(e.Name) || e.Size < 0 || e.Size > MaxTotal-total {
			return 0, errors.New("剪贴板文件名或大小不受支持")
		}
		total += e.Size
	}
	return total, nil
}

func CheckPNG(b []byte) error {
	if len(b) == 0 || len(b) > MaxImage {
		return errors.New("剪贴板图片超过 32 MiB 或为空")
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(b))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width) > MaxPixels/int64(cfg.Height) {
		return errors.New("无效或像素过多的剪贴板图片")
	}
	return nil
}

type Sender struct {
	meta          Manifest
	reader        io.Reader
	files         []*os.File
	offset, total int64
	started       bool
}

func NewSender(c Content) (*Sender, error) {
	s := &Sender{}
	if len(c.Files) == 0 {
		if err := CheckPNG(c.PNG); err != nil {
			return nil, err
		}
		s.meta = Manifest{"png", []Entry{{"clipboard.png", int64(len(c.PNG))}}}
		s.total = int64(len(c.PNG))
		s.reader = bytes.NewReader(c.PNG)
		return s, nil
	}
	if len(c.Files) > MaxFiles {
		return nil, errors.New("一次最多共享 128 个文件")
	}
	s.meta.Kind = "files"
	var readers []io.Reader
	for _, path := range c.Files {
		fi, err := os.Lstat(path)
		if err != nil || !filepath.IsAbs(path) || !fi.Mode().IsRegular() {
			s.Close()
			return nil, errors.New("只能共享可读取的普通文件")
		}
		f, err := os.Open(path)
		if err != nil {
			s.Close()
			return nil, err
		}
		s.files = append(s.files, f)
		st, err := f.Stat()
		if err != nil || !os.SameFile(fi, st) {
			s.Close()
			return nil, errors.New("复制的源文件已改变")
		}
		s.meta.Entries = append(s.meta.Entries, Entry{filepath.Base(path), st.Size()})
		// A shrinking file must fail rather than borrow bytes from the next file.
		readers = append(readers, &exactReader{r: io.LimitedReader{R: f, N: st.Size()}})
	}
	total, err := s.meta.size()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.total = total
	s.reader = io.MultiReader(readers...)
	return s, nil
}

type exactReader struct{ r io.LimitedReader }

func (r *exactReader) Read(b []byte) (int, error) {
	n, e := r.r.Read(b)
	if e == io.EOF && r.r.N != 0 {
		e = io.ErrUnexpectedEOF
	}
	return n, e
}

func (s *Sender) Next() (*Part, bool, error) {
	p := &Part{Offset: s.offset, Bytes: make([]byte, min(ChunkSize, s.total-s.offset))}
	if !s.started {
		p.Manifest = &s.meta
		s.started = true
	}
	if _, err := io.ReadFull(s.reader, p.Bytes); err != nil {
		s.Close()
		return nil, false, err
	}
	s.offset += int64(len(p.Bytes))
	done := s.offset == s.total
	if done {
		s.Close()
	}
	return p, done, nil
}
func (s *Sender) Close() {
	for _, f := range s.files {
		f.Close()
	}
	s.files = nil
}

type Receiver struct {
	meta          *Manifest
	total, offset int64
	dir           string
	files         []string
	index         int
	remaining     int64
	out           *os.File
	image         bytes.Buffer
}

func cacheRoot() (string, error) {
	base := os.Getenv("NSGO_CACHE_DIR")
	if base == "" {
		var err error
		base, err = os.UserCacheDir()
		if err != nil {
			return "", err
		}
	}
	root := filepath.Join(base, "remote-go", "clipboard")
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	return root, nil
}

func (r *Receiver) Close() {
	if r.out != nil {
		r.out.Close()
	}
	if r.dir != "" {
		os.RemoveAll(r.dir)
	}
	*r = Receiver{}
}

// Publish transfers ownership of the cache directory to the clipboard. A
// successful paste may happen after the session has been disconnected.
func (r *Receiver) Published() { r.dir = ""; r.Close() }

func (r *Receiver) start(m *Manifest) error {
	if r.meta != nil {
		return errors.New("重复的剪贴板传输起点")
	}
	total, err := m.size()
	if err != nil {
		return err
	}
	r.meta = m
	r.total = total
	if m.Kind == "png" {
		return nil
	}
	root, err := cacheRoot()
	if err != nil {
		return err
	}
	r.dir, err = os.MkdirTemp(root, "batch-")
	if err != nil {
		return err
	}
	for i, e := range m.Entries {
		// Separate directories preserve identical names from different sources.
		dir := filepath.Join(r.dir, fmt.Sprint(i))
		if err = os.Mkdir(dir, 0700); err != nil {
			return err
		}
		path := filepath.Join(dir, e.Name)
		f, err := os.OpenFile(path, os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if err = f.Close(); err != nil {
			return err
		}
		r.files = append(r.files, path)
	}
	return nil
}

func (r *Receiver) Accept(p *Part) (Content, bool, error) {
	c, done, err := r.accept(p)
	if err != nil {
		r.Close()
	}
	return c, done, err
}
func (r *Receiver) accept(p *Part) (Content, bool, error) {
	if p.Manifest != nil {
		if err := r.start(p.Manifest); err != nil {
			return Content{}, false, err
		}
	}
	if r.meta == nil || p.Offset != r.offset || len(p.Bytes) > ChunkSize || int64(len(p.Bytes)) > r.total-r.offset || len(p.Bytes) == 0 && r.offset != r.total {
		return Content{}, false, errors.New("剪贴板分块偏移或长度不正确")
	}
	b := p.Bytes
	if r.meta.Kind == "png" {
		r.image.Write(b)
	} else {
		for len(b) > 0 {
			if r.out == nil {
				for r.index < len(r.files) && r.meta.Entries[r.index].Size == 0 {
					r.index++
				}
				f, err := os.OpenFile(r.files[r.index], os.O_WRONLY, 0)
				if err != nil {
					return Content{}, false, err
				}
				r.out = f
				r.remaining = r.meta.Entries[r.index].Size
			}
			n := min(int64(len(b)), r.remaining)
			if _, err := r.out.Write(b[:n]); err != nil {
				return Content{}, false, err
			}
			b = b[n:]
			r.remaining -= n
			if r.remaining == 0 {
				err := r.out.Close()
				r.out = nil
				if err != nil {
					return Content{}, false, err
				}
				r.index++
			}
		}
	}
	r.offset += int64(len(p.Bytes))
	if r.offset != r.total {
		return Content{}, false, nil
	}
	if r.meta.Kind == "png" {
		if err := CheckPNG(r.image.Bytes()); err != nil {
			return Content{}, false, err
		}
		return Content{PNG: r.image.Bytes()}, true, nil
	}
	return Content{Files: r.files}, true, nil
}

// Sweep only our own old batches. Keep any paths currently on the clipboard.
func sweep(keep []string) {
	root, err := cacheRoot()
	if err != nil {
		return
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "batch-") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		protected := false
		for _, p := range keep {
			if strings.HasPrefix(filepath.Clean(p), dir+string(os.PathSeparator)) {
				protected = true
				break
			}
		}
		if st, err := e.Info(); !protected && err == nil && time.Since(st.ModTime()) > 7*24*time.Hour {
			os.RemoveAll(dir)
		}
	}
}
