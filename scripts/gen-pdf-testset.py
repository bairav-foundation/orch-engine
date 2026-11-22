#!/usr/bin/env python3
"""Generate the PDF test set for `attachments: scan` (fake data only).

Text PDFs (the text is in the file, like an exported statement), scanned
PDFs (each page is a picture), and mixed / multi-page ones where the
sensitive value is on a later page. Values are the same fakes as the image
test set: published test card numbers, AWS's example key, a made-up
Aadhaar with a valid checksum, example.com emails.

  python3 scripts/gen-pdf-testset.py   # writes cli/internal/dlp/testdata/pdf/
Needs reportlab, Pillow and the DejaVu fonts.
"""
import io, json, os
from PIL import Image, ImageDraw, ImageFont
from reportlab.lib.pagesizes import A4
from reportlab.pdfgen import canvas

OUT = os.path.join(os.path.dirname(__file__), "..", "cli", "internal", "dlp", "testdata", "pdf")
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf"
FILLER = ["Thank you for banking with us.", "Your statement for October is below.", "Questions? Visit your nearest branch."]

def text_page(c, lines):
    y = 780
    for l in lines:
        c.setFont("Helvetica", 12)
        c.drawString(60, y, l)
        y -= 22
    c.showPage()

def scanned_page(c, lines, dark=False):
    f = ImageFont.truetype(FONT, 34)
    bg, fg = ((30, 30, 30), (230, 230, 230)) if dark else ((248, 246, 240), (25, 25, 25))
    im = Image.new("RGB", (1654, 2339), bg)  # A4 at 200 dpi
    d = ImageDraw.Draw(im)
    for i, l in enumerate(lines):
        d.text((140, 180 + i * 60), l, font=f, fill=fg)
    im = im.rotate(0.8, expand=False, fillcolor=bg)
    buf = io.BytesIO()
    im.save(buf, "JPEG", quality=70)
    buf.seek(0)
    from reportlab.lib.utils import ImageReader
    c.drawImage(ImageReader(buf), 0, 0, width=A4[0], height=A4[1])
    c.showPage()

CASES = [
    # name, pages: list of (kind, lines), labels
    ("statement-card", [("text", ["Card statement", "Card number 4111 1111 1111 1111", "Balance due 12,400.00"] + FILLER)], ["CREDIT_CARD"]),
    ("kyc-aadhaar", [("text", ["KYC form", "Name: Test User", "Aadhaar number 2345 6789 0124"])], ["AADHAAR"]),
    ("contract-email", [("text", ["Service agreement", "Contact: priya.k@example.com", "Term: 12 months"])], ["EMAIL"]),
    ("scan-card", [("scan", ["Card statement", "Card number", "5555 5555 5555 4444"])], ["CREDIT_CARD"]),
    ("scan-aws", [("scan", ["Security credentials", "Access key ID", "AKIAIOSFODNN7EXAMPLE"])], ["AWS_ACCESS_KEY"]),
    ("scan-dark-ssn", [("scan-dark", ["Employee onboarding", "SSN: 219-09-9999"])], ["SSN"]),
    ("multipage-late-card", [("text", FILLER), ("text", ["Annexure", "Policy terms apply."]), ("text", ["Payment", "Card 4111 1111 1111 1111"])], ["CREDIT_CARD"]),
    ("mixed-scan-page-key", [("text", ["Cover letter", "Please find the attached credentials page."] + FILLER), ("scan", ["Access key ID", "AKIAIOSFODNN7EXAMPLE"])], ["AWS_ACCESS_KEY"]),
    ("clean-report", [("text", ["Weekly sales report", "Revenue up 12% on last quarter", "Next review on Monday"] + FILLER)], []),
    ("clean-scan-minutes", [("scan", ["Team sync minutes", "Room 4B at 10:30", "Agenda: roadmap, hiring"])], []),
]

def main():
    os.makedirs(OUT, exist_ok=True)
    manifest = []
    for name, pages, labels in CASES:
        path = os.path.join(OUT, name + ".pdf")
        c = canvas.Canvas(path, pagesize=A4, invariant=1)  # invariant: same bytes every run
        for kind, lines in pages:
            if kind == "text":
                text_page(c, lines)
            else:
                scanned_page(c, lines, dark=(kind == "scan-dark"))
        c.save()
        manifest.append({"file": name + ".pdf", "labels": labels})
    with open(os.path.join(OUT, "manifest.json"), "w") as fh:
        json.dump(manifest, fh, indent=1)
    print(f"{len(manifest)} PDFs in {os.path.relpath(OUT)}")

if __name__ == "__main__":
    main()
