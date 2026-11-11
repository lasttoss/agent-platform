"""Generate the OCR test fixtures.

They are generated instead of committed for one reason: a real sample is a real invoice,
with a real name and a real amount on it. Fixtures in a repo must be synthetic.
Run: python3 services/ocr/samples/make_samples.py
"""
from __future__ import annotations

from pathlib import Path

from PIL import Image, ImageDraw

HERE = Path(__file__).resolve().parent

INVOICE = [
    "INVOICE 2026-11-A",
    "Issued to ACME Demo Ltd",
    "Backend engineering services, November 2026",
    "Subtotal 1,234,000 VND - VAT 10 percent - Total 1,357,400 VND",
    "Payment due within 15 days of the issue date.",
]


def _pdf_with_text_layer(lines: list[str]) -> bytes:
    """A minimal PDF with a real text layer (Helvetica), written by hand.

    Pillow can save a PDF but rasterises it, which would make every "scanned document"
    test pass for the wrong reason — the parser must find actual text here.
    """
    def esc(t: str) -> str:
        return t.replace("\\", r"\\").replace("(", r"\(").replace(")", r"\)")

    content = ["BT", "/F1 14 Tf", "60 700 Td", "18 TL"]
    for line in lines:
        content.append(f"({esc(line)}) Tj T*")
    content.append("ET")
    stream = "\n".join(content).encode()

    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] "
        b"/Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
        b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for i, body in enumerate(objs, start=1):
        offsets.append(len(out))
        out += f"{i} 0 obj\n".encode() + body + b"\nendobj\n"
    xref_at = len(out)
    out += f"xref\n0 {len(objs) + 1}\n".encode() + b"0000000000 65535 f \n"
    for off in offsets:
        out += f"{off:010d} 00000 n \n".encode()
    out += (f"trailer\n<< /Size {len(objs) + 1} /Root 1 0 R >>\nstartxref\n{xref_at}\n%%EOF\n").encode()
    return bytes(out)


def text_pdf() -> None:
    (HERE / "text.pdf").write_bytes(_pdf_with_text_layer(INVOICE))


def scan_pdf() -> None:
    # no text layer at all: this is what a scan looks like to a parser
    img = Image.new("L", (1240, 400), 235)
    draw = ImageDraw.Draw(img)
    draw.rectangle([40, 40, 1200, 360], outline=180)
    img.save(HERE / "scan.pdf", "PDF", resolution=150.0)


def blank_png() -> None:
    Image.new("RGB", (300, 120), "white").save(HERE / "blank.png")


if __name__ == "__main__":
    text_pdf()
    scan_pdf()
    blank_png()
    print("samples regenerated with synthetic content")
