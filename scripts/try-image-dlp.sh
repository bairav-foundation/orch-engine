#!/usr/bin/env bash
# Send an image or PDF through a DLP proxy the way a chat client would
# (inline, OpenAI chat format) and print the answer.
#
#   scripts/try-image-dlp.sh cli/internal/dlp/testdata/ocr/card-checkout--plain.png
#   scripts/try-image-dlp.sh cli/internal/dlp/testdata/pdf/scan-aws.pdf
#   scripts/try-image-dlp.sh my-screenshot.png http://127.0.0.1:8090/v1
#
# With `bairav dlp serve --attachments scan` running, a card number, key,
# email or ID in the image comes back as dlp_image_blocked.
set -euo pipefail
if [ $# -lt 1 ]; then
  echo "usage: $0 IMAGE [PROXY_URL]" >&2
  echo "  e.g. $0 cli/internal/dlp/testdata/ocr/card-checkout--plain.png" >&2
  exit 2
fi
img=$1
url=${2:-http://127.0.0.1:8090/v1}
[ -s "$img" ] || { echo "no such image (or empty): $img" >&2; exit 1; }
lower=$(printf %s "$img" | tr "[:upper:]" "[:lower:]")  # bash 3 on macOS: no ${img,,}
case "$lower" in
  *.png) mime=image/png ;; *.jpg|*.jpeg) mime=image/jpeg ;; *.webp) mime=image/webp ;; *.gif) mime=image/gif ;;
  *.pdf) mime=application/pdf ;;
  *) echo "use a .png, .jpg, .webp, .gif or .pdf" >&2; exit 1 ;;
esac
body=$(mktemp)
trap 'rm -f "$body"' EXIT
{
  if [ "$mime" = application/pdf ]; then
    printf '{"messages":[{"role":"user","content":[{"type":"text","text":"Summarise this document."},'
    printf '{"type":"file","file":{"filename":"%s","file_data":"data:application/pdf;base64,' "$(basename "$img")"
  else
    printf '{"messages":[{"role":"user","content":[{"type":"text","text":"What does this image say?"},'
    printf '{"type":"image_url","image_url":{"url":"data:%s;base64,' "$mime"
  fi
  base64 < "$img" | tr -d '\n'
  printf '"}}]}]}'
} > "$body"
echo "sending $img ($(wc -c < "$img" | tr -d ' ') bytes) to $url"
curl -sS "$url/chat/completions" -H 'content-type: application/json' --data-binary @"$body"
echo
