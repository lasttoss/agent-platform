from __future__ import annotations

import json
import sys
import threading
import unittest
import urllib.error
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from server import make_server  # noqa: E402

HERE = Path(__file__).resolve().parent.parent
SAMPLES = HERE / "samples"


def _post(port: int, payload: dict) -> tuple[int, dict]:
    req = urllib.request.Request(f"http://127.0.0.1:{port}/v1/ocr",
                                 data=json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=30) as r:
            return r.status, json.loads(r.read().decode())
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode())


class ToolServiceTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        SAMPLES.mkdir(exist_ok=True)
        # text-layer PDF generated from HTML-free raw PDF content would be fragile,
        # so build a minimal one with pypdf-free bytes: reuse any PDF already here.
        cls.text_pdf = SAMPLES / "text.pdf"
        if not cls.text_pdf.exists():
            _write_min_text_pdf(cls.text_pdf)
        cls.scan_pdf = SAMPLES / "scan.pdf"
        if not cls.scan_pdf.exists():
            _write_image_only_pdf(cls.scan_pdf)
        cls.blank = SAMPLES / "blank.png"
        if not cls.blank.exists():
            _write_blank_png(cls.blank)
        cls.srv = make_server(port=0)
        cls.port = cls.srv.server_address[1]
        threading.Thread(target=cls.srv.serve_forever, daemon=True).start()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.srv.shutdown()
        cls.srv.server_close()

    def test_health_reports_engines(self):
        with urllib.request.urlopen(f"http://127.0.0.1:{self.port}/health", timeout=10) as r:
            body = json.loads(r.read().decode())
        self.assertTrue(body["ok"])
        self.assertIn("pdf", body["engines"])
        self.assertIn("image_ocr_available", body["engines"])

    def test_text_pdf_returns_text(self):
        code, body = _post(self.port, {"path": "samples/text.pdf"})
        self.assertEqual(code, 200)
        self.assertFalse(body["scanned"])
        self.assertGreater(body["chars"], 40)
        self.assertIn("INVOICE", body["text"].upper())
        self.assertIn("elapsed_ms", body)

    def test_scan_is_flagged_not_silently_empty(self):
        code, body = _post(self.port, {"path": "samples/scan.pdf"})
        self.assertEqual(code, 200)
        self.assertTrue(body["scanned"])
        self.assertEqual(body["warning"], "no text layer: needs an OCR engine")

    def test_path_escape_is_refused(self):
        code, body = _post(self.port, {"path": "../../../etc/passwd"})
        self.assertIn(code, (400, 403))
        self.assertIn(body["error"], ("path_outside_root", "suffix_not_allowed:.", "not_a_file"))

    def test_absolute_path_outside_root_is_refused(self):
        code, body = _post(self.port, {"path": "/etc/hostname"})
        self.assertEqual(code, 403)
        self.assertEqual(body["error"], "path_outside_root")

    def test_missing_file_is_404(self):
        code, body = _post(self.port, {"path": "samples/nope.pdf"})
        self.assertEqual(code, 404)

    def test_image_without_engine_reports_gap(self):
        code, body = _post(self.port, {"path": "samples/blank.png"})
        if code == 503:
            self.assertEqual(body["error"], "ocr_engine_missing")
        else:
            self.assertEqual(code, 200)  # engine present in this environment


def _write_min_text_pdf(path: Path) -> None:
    """A tiny one-page PDF with a real text object (no external libs needed)."""
    lines = [
        ("INVOICE 2026-11-A", 18, 700),
        ("Issued to ACME Demo Ltd", 11, 670),
        ("Backend engineering services, November 2026", 11, 650),
        ("Subtotal 1,234,000 VND - VAT 10 percent - Total 1,357,400 VND", 11, 630),
        ("Payment due within 15 days of the issue date.", 11, 610),
    ]
    parts = [b"BT"]
    for text, size, y in lines:
        # Tm is absolute; repeated Td would accumulate and push later lines off-page.
        parts.append(f"/F1 {size} Tf 1 0 0 1 72 {y} Tm ({text}) Tj".encode())
    parts.append(b"ET")
    stream = b"\n".join(parts)
    objs = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] /Resources << /Font << /F1 5 0 R >> >> /Contents 4 0 R >>",
        b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
    ]
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for i, body in enumerate(objs, start=1):
        offsets.append(len(out))
        out += f"{i} 0 obj\n".encode() + body + b"\nendobj\n"
    xref = len(out)
    out += f"xref\n0 {len(objs)+1}\n".encode() + b"0000000000 65535 f \n"
    for off in offsets:
        out += f"{off:010d} 00000 n \n".encode()
    out += (f"trailer\n<< /Size {len(objs)+1} /Root 1 0 R >>\nstartxref\n{xref}\n%%EOF\n").encode()
    path.write_bytes(bytes(out))


def _write_image_only_pdf(path: Path) -> None:
    from PIL import Image
    img = Image.new("RGB", (400, 300), (255, 255, 255))
    img.save(path, "PDF", resolution=150)


def _write_blank_png(path: Path) -> None:
    from PIL import Image
    Image.new("RGB", (120, 60), (255, 255, 255)).save(path)


if __name__ == "__main__":
    unittest.main(verbosity=2)
