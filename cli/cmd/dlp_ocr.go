// Copyright 2026 BAIRAV Foundation
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/bairav-foundation/orch-engine/cli/internal/dlp"
)

var dlpOCRCmd = &cobra.Command{
	Use:   "ocr",
	Short: "Run the OCR server that `dlp serve --attachments scan` reads images with",
	Long: `Serves POST /ocr (image in, text out) with Tesseract, for the DLP proxy's
attachments scan mode. Runs next to the proxy and listens on loopback: the
image and its text never leave the machine or Pod, and nothing is logged.

On Kubernetes the operator runs it for you (the dlp-ocr image) when a
DLPPolicy sets attachments: scan. Locally it needs tesseract:

  brew install tesseract            # macOS
  sudo apt install tesseract-ocr    # Debian, Ubuntu
  bairav dlp ocr &
  bairav dlp serve --attachments scan`,
	Args: cobra.NoArgs,
	RunE: runDLPOCR,
}

func init() {
	dlpOCRCmd.Flags().String("listen", "127.0.0.1:8095", "address to listen on (env BAIRAV_OCR_LISTEN)")
	dlpOCRCmd.Flags().String("langs", "eng", "Tesseract languages, e.g. eng or eng+hin (env BAIRAV_OCR_LANGS)")
}

func runDLPOCR(cmd *cobra.Command, _ []string) error {
	for flag, env := range map[string]string{"listen": "BAIRAV_OCR_LISTEN", "langs": "BAIRAV_OCR_LANGS"} {
		if v := os.Getenv(env); v != "" && !cmd.Flags().Changed(flag) {
			_ = cmd.Flags().Set(flag, v)
		}
	}
	listen, _ := cmd.Flags().GetString("listen")
	langs, _ := cmd.Flags().GetString("langs")
	if _, err := exec.LookPath("tesseract"); err != nil {
		return errors.New("tesseract not found: brew install tesseract (macOS) or apt install tesseract-ocr, or run the ghcr.io/bairav-foundation/dlp-ocr image")
	}
	srv := &http.Server{
		Addr:              listen,
		Handler:           dlp.NewOCRServer(langs),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      90 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		sctx, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Fprintf(cmd.OutOrStdout(), "bairav dlp ocr %s\n  listening: http://%s (POST /ocr, GET /healthz)\n  languages: %s\n", version, listen, langs)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
