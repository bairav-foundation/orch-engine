// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package dlp

// OCR for attachments: scan.
//
// The DLP proxy stays a small static binary; reading text out of images
// needs Tesseract, which lives in its own container (images/dlp-ocr) next
// to the proxy and runs `bairav dlp ocr`. The proxy posts the image bytes
// to it over loopback and gets the text back. Nothing leaves the Pod, and
// the text is only held in memory for the scan: it is never logged.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // decoders for prepareForOCR
	_ "image/jpeg"
	"image/png"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// OCR reads the text in an image.
type OCR interface {
	Text(ctx context.Context, mime string, image []byte) (string, error)
}

// MaxOCRBytes is the largest image the proxy sends for OCR (decoded).
const MaxOCRBytes = 10 << 20

// maxOCRPixels caps an image's size in pixels (40 MP: a phone photo is
// 12-50 MP, a 4K screenshot 8 MP).
const maxOCRPixels = 40_000_000

// ocrTimeout bounds one image; a large screenshot takes 1-3 s.
const ocrTimeout = 30 * time.Second

// HTTPOCR is the proxy's client for a `bairav dlp ocr` server.
type HTTPOCR struct {
	URL    string // base URL, e.g. http://127.0.0.1:8095
	Client *http.Client
}

// NewHTTPOCR returns a client for the OCR server at baseURL.
func NewHTTPOCR(baseURL string) *HTTPOCR {
	// PDFs can take up to pdfTimeout; leave room for the reply.
	return &HTTPOCR{URL: strings.TrimRight(baseURL, "/"), Client: &http.Client{Timeout: pdfTimeout + 15*time.Second}}
}

type ocrReply struct {
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}

// Text implements OCR.
func (o *HTTPOCR) Text(ctx context.Context, mime string, image []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.URL+"/ocr", bytes.NewReader(image))
	if err != nil {
		return "", err
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	req.Header.Set("Content-Type", mime)
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ocr: %w", err)
	}
	defer resp.Body.Close()
	var r ocrReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return "", fmt.Errorf("ocr: HTTP %d, unreadable reply", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ocr: HTTP %d: %s", resp.StatusCode, r.Error)
	}
	return r.Text, nil
}

// OCRServer serves POST /ocr (image or PDF bytes in, {"text": ...} out)
// with Tesseract and poppler, and GET /healthz.
type OCRServer struct {
	// Langs are Tesseract language codes, e.g. "eng" or "eng+hin".
	Langs string
	// Run runs tesseract with the image on stdin and returns stdout;
	// replaced in tests.
	Run func(ctx context.Context, stdin []byte, args ...string) ([]byte, error)
	// Tool runs a poppler tool (pdfinfo, pdftotext, pdftoppm) for PDFs;
	// nil = the real binaries.
	Tool func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// NewOCRServer returns a server that runs the tesseract binary.
func NewOCRServer(langs string) *OCRServer {
	if langs == "" {
		langs = "eng"
	}
	return &OCRServer{Langs: langs, Run: runTesseract}
}

func runTesseract(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tesseract", args...) //nolint:gosec // fixed binary; args are fixed flags and a validated language list
	cmd.Stdin = bytes.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.Error
		if errors.As(err, &ee) {
			return nil, fmt.Errorf("tesseract not found: install it (brew install tesseract, apt install tesseract-ocr) or use the dlp-ocr image")
		}
		return nil, fmt.Errorf("tesseract: %v: %s", err, clipText(stderr.String(), 200))
	}
	return out, nil
}

// imageTypes are what Tesseract (via Leptonica) reads.
var imageTypes = map[string]bool{
	"image/png": true, "image/jpeg": true, "image/jpg": true, "image/webp": true,
	"image/gif": true, "image/bmp": true, "image/tiff": true, "application/octet-stream": true,
}

// ServeHTTP implements http.Handler.
func (s *OCRServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	case r.URL.Path != "/ocr" || r.Method != http.MethodPost:
		writeOCRError(w, http.StatusNotFound, "only POST /ocr and GET /healthz are served")
		return
	}
	mt := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0]))
	if mt == "" {
		mt = "application/octet-stream"
	}
	isPDF := mt == "application/pdf"
	if !imageTypes[mt] && !isPDF {
		writeOCRError(w, http.StatusUnsupportedMediaType, "not an image or PDF: "+mt)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxOCRBytes))
	if err != nil {
		writeOCRError(w, http.StatusRequestEntityTooLarge, "image over the size limit")
		return
	}
	// Check the pixel count from the header before anything decodes it: a
	// small PNG can claim 50000x50000 pixels and exhaust memory.
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(body)); err == nil && cfg.Width*cfg.Height > maxOCRPixels {
		writeOCRError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("image is %dx%d pixels, over the %d MP limit", cfg.Width, cfg.Height, maxOCRPixels/1_000_000))
		return
	}
	if !validLangs(s.Langs) {
		writeOCRError(w, http.StatusInternalServerError, "bad language list")
		return
	}
	if isPDF {
		text, err := s.pdfText(r.Context(), body)
		if err != nil {
			writeOCRError(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ocrReply{Text: text})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ocrTimeout)
	defer cancel()
	// --psm 3: fully automatic page segmentation, the right default for
	// screenshots and photos of documents.
	out, err := s.Run(ctx, prepareForOCR(body), "stdin", "stdout", "-l", s.Langs, "--psm", "3")
	if err != nil {
		writeOCRError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(ocrReply{Text: string(out)})
}

// prepareForOCR helps Tesseract with two things screenshots get wrong:
//   - dark mode (light text on a dark background) is inverted to
//     dark-on-light: on the test set, dark images went from 4 of 6 found
//     to all of them;
//   - small images are scaled up 2x, since Tesseract wants text around
//     20-30 px high: 11 px text went from 4 of 6 to 6 of 6.
//
// Formats Go can't decode (WebP, TIFF) go to Tesseract unchanged.
func prepareForOCR(data []byte) []byte {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w*h == 0 || w*h > maxOCRPixels {
		return data
	}
	var sum, n uint64
	for y := b.Min.Y; y < b.Max.Y; y += 4 {
		for x := b.Min.X; x < b.Max.X; x += 4 {
			sum += uint64(color.GrayModel.Convert(img.At(x, y)).(color.Gray).Y)
			n++
		}
	}
	invert := n > 0 && sum/n < 110
	scale := 1
	if w < 1200 && w*h < 10_000_000/4 {
		scale = 2
	}
	if !invert && scale == 1 {
		return data
	}
	out := image.NewGray(image.Rect(0, 0, w*scale, h*scale))
	for y := 0; y < h*scale; y++ {
		for x := 0; x < w*scale; x++ {
			g := color.GrayModel.Convert(img.At(b.Min.X+x/scale, b.Min.Y+y/scale)).(color.Gray).Y
			if invert {
				g = 255 - g
			}
			out.Pix[y*out.Stride+x] = g
		}
	}
	var buf bytes.Buffer
	if png.Encode(&buf, out) != nil {
		return data
	}
	return buf.Bytes()
}

// validLangs accepts "eng", "eng+hin": letters, underscores and pluses only.
func validLangs(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_', c == '+':
		default:
			return false
		}
	}
	return true
}

func writeOCRError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(ocrReply{Error: msg})
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
