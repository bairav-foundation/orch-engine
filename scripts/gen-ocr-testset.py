#!/usr/bin/env python3
"""Generate the OCR test set for `attachments: scan` (fake data only).

Every value is fake: card numbers are the networks' published test
numbers, the AWS key is AWS's documented example, the Aadhaar number is a
made-up value with a valid checksum, emails use example.com. Each image is
rendered several ways (sizes, dark mode, low contrast, JPEG, slight tilt)
so the detection rate means something.

  python3 scripts/gen-ocr-testset.py   # writes cli/internal/dlp/testdata/ocr/
Needs Pillow and the DejaVu fonts.
"""
import json, os, sys
from PIL import Image, ImageDraw, ImageFont, ImageFilter

OUT = os.path.join(os.path.dirname(__file__), "..", "cli", "internal", "dlp", "testdata", "ocr")
FONTS = ["/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf", "/usr/share/fonts/truetype/dejavu/DejaVuSerif.ttf"]

# name, lines, labels the DLP must find (empty = must find nothing)
CASES = [
    ("card-checkout", ["Payment details", "Card number", "4111 1111 1111 1111", "Expiry 12/29   CVV ***"], ["CREDIT_CARD"]),
    ("card-mastercard", ["Saved card", "5555 5555 5555 4444", "Name on card: Test User"], ["CREDIT_CARD"]),
    ("email-crm", ["Customer record", "Name: Priya K", "Email: priya.k@example.com", "Plan: Gold"], ["EMAIL"]),
    ("aws-console", ["Security credentials", "Access key ID", "AKIAIOSFODNN7EXAMPLE", "Status: Active"], ["AWS_ACCESS_KEY"]),
    ("aadhaar-form", ["KYC form", "Aadhaar number", "2345 6789 0124", "Verified"], ["AADHAAR"]),
    ("ssn-form", ["Employee onboarding", "SSN: 219-09-9999", "Start date: 3 Jan"], ["SSN"]),
    ("clean-report", ["Weekly sales report", "Revenue up 12% on last quarter", "Next review on Monday"], []),
    ("clean-meeting", ["Team sync", "Room 4B at 10:30", "Agenda: roadmap, hiring"], []),
]

VARIANTS = {
    "plain":   dict(size=28, fg=(20, 20, 20), bg=(255, 255, 255)),
    "small":   dict(size=16, fg=(20, 20, 20), bg=(255, 255, 255)),
    "dark":    dict(size=26, fg=(230, 230, 230), bg=(30, 32, 36)),
    "lowcon":  dict(size=26, fg=(110, 110, 110), bg=(205, 205, 205)),
    "jpeg":    dict(size=24, fg=(20, 20, 20), bg=(250, 250, 250), jpeg=True),
    "tilt":    dict(size=26, fg=(20, 20, 20), bg=(255, 255, 255), tilt=3),
    "serif":   dict(size=26, fg=(20, 20, 20), bg=(255, 255, 255), font=1),
    # Harder: what a phone photo of a screen or a resized screenshot does.
    "blur":    dict(size=22, fg=(30, 30, 30), bg=(245, 245, 245), blur=1.3),
    "noise":   dict(size=22, fg=(30, 30, 30), bg=(235, 235, 235), noise=40, jpeg=True),
    "tiny":    dict(size=11, fg=(20, 20, 20), bg=(255, 255, 255)),
}

def render(lines, size, fg, bg, font=0, jpeg=False, tilt=0, blur=0, noise=0):
    f = ImageFont.truetype(FONTS[font], size)
    w = max(int(f.getlength(l)) for l in lines) + 80
    h = (size + 14) * len(lines) + 80
    im = Image.new("RGB", (w, h), bg)
    d = ImageDraw.Draw(im)
    for i, l in enumerate(lines):
        d.text((40, 40 + i * (size + 14)), l, font=f, fill=fg)
    if tilt:
        im = im.rotate(tilt, expand=True, fillcolor=bg, resample=Image.BICUBIC)
    if blur:
        im = im.filter(ImageFilter.GaussianBlur(blur))
    if noise:
        import random
        rnd = random.Random(7)  # same noise every run
        px = im.load()
        for _ in range(im.size[0] * im.size[1] // 6):
            x, y = rnd.randrange(im.size[0]), rnd.randrange(im.size[1])
            r, g, b = px[x, y]
            d = rnd.randint(-noise, noise)
            px[x, y] = (max(0, min(255, r + d)), max(0, min(255, g + d)), max(0, min(255, b + d)))
    return im, ("jpg" if jpeg else "png")

def main():
    os.makedirs(OUT, exist_ok=True)
    manifest = []
    for name, lines, labels in CASES:
        for v, opts in VARIANTS.items():
            im, ext = render(lines, **opts)
            fn = f"{name}--{v}.{ext}"
            path = os.path.join(OUT, fn)
            if ext == "jpg":
                im.save(path, quality=60)
            else:
                im.save(path, optimize=True)
            manifest.append({"file": fn, "labels": labels})
    with open(os.path.join(OUT, "manifest.json"), "w") as fh:
        json.dump(manifest, fh, indent=1)
    print(f"{len(manifest)} images in {os.path.relpath(OUT)}")

if __name__ == "__main__":
    sys.exit(main())
