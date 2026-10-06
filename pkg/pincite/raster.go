package pincite

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
)

// Rasterizer renders PDF pages to images.
type Rasterizer interface {
	// RenderPages renders pages from..to (1-based, inclusive) and calls
	// emit once per page with its JPEG bytes.
	RenderPages(ctx context.Context, pdf []byte, from, to int, emit func(page int, jpeg []byte) error) error
	// Available reports whether the renderer can run here.
	Available() bool
}

// Poppler renders pages with poppler's pdftoppm (HAL-834).
//
// Page images are rendered out of process, never with CGo: the engine
// binary stays pure Go, and the renderer is a program the runtime image
// carries (poppler-utils). It runs on ingest and, for documents ingested
// before page images existed, once per page on first view; a rendered
// page is stored and never rendered again. No query ever waits on it.
//
// JPEG rather than the WebP HAL-834 named: pdftoppm writes JPEG
// natively, and a 144 DPI text page is ~100–200 KB either way, inside
// the budget that issue accepted.
type Poppler struct {
	Bin     string // default "pdftoppm"
	DPI     int    // default 144
	Quality int    // JPEG quality, default 82
}

func (p Poppler) bin() string {
	if p.Bin != "" {
		return p.Bin
	}
	return "pdftoppm"
}

// Available reports whether pdftoppm is on PATH.
func (p Poppler) Available() bool {
	_, err := exec.LookPath(p.bin())
	return err == nil
}

var pageFileRe = regexp.MustCompile(`-(\d+)\.jpg$`)

// RenderPages runs pdftoppm once over the whole range — one process,
// not one per page — and emits each page as it is read back.
func (p Poppler) RenderPages(ctx context.Context, pdf []byte, from, to int, emit func(int, []byte) error) error {
	if from < 1 || to < from {
		return fmt.Errorf("pincite: bad page range %d-%d", from, to)
	}
	dpi := p.DPI
	if dpi <= 0 {
		dpi = 144
	}
	q := p.Quality
	if q <= 0 || q > 100 {
		q = 82
	}
	dir, err := os.MkdirTemp("", "vls-raster-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	in := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(in, pdf, 0o600); err != nil {
		return err
	}
	prefix := filepath.Join(dir, "p")
	cmd := exec.CommandContext(ctx, p.bin(),
		"-r", strconv.Itoa(dpi),
		"-jpeg", "-jpegopt", "quality="+strconv.Itoa(q),
		"-f", strconv.Itoa(from), "-l", strconv.Itoa(to),
		in, prefix)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("pincite: pdftoppm: %w: %s", err, bytes.TrimSpace(stderr.Bytes()))
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		m := pageFileRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[1])
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		if err := emit(n, b); err != nil {
			return err
		}
	}
	return nil
}
