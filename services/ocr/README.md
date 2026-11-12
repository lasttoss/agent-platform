# ocr tool-service

One tool, one contract, hard guards. This is the "worker" in
`agent-gateway → HTTP → tool service`; it has no idea who is calling and no authority.

## Two ways in: now, and "queue it"

A scan takes 2–5 s per file, so ten files in one HTTP call is a 30–50 s request that times out
somewhere in the middle and loses the work. The queue makes progress durable:

```
POST /v1/ocr      {"path": "samples/x.pdf"}          → 200 with the result (one file, fast path)
POST /v1/jobs     {"path": "samples/x.pdf"}          → 202 {"id", "status": "queued", "poll"}
GET  /v1/jobs/<id>                                    → status: queued | running | done | failed | expired
```

Retention is part of the contract, not an afterthought: extracted text is somebody else's
document, so it is deleted after `JOBS_TTL_S` (default 3600) and the job row becomes `expired`
with a note — the caller learns "the text is gone", never "job not found". A full queue answers
`503 queue_full` and marks the job rejected; nothing is silently dropped.

## Contract

```
POST /v1/ocr
{ "path": "samples/text.pdf", "lang": "eng" }

200 { "text": "...", "chars": 412, "pages": 1, "engine": "pdftotext",
      "scanned": false, "warning": null, "file": "samples/text.pdf", "elapsed_ms": 12.4 }
503 { "error": "ocr_engine_missing", "detail": "tesseract is not installed ..." }
403 { "error": "path_outside_root" }
404 { "error": "not_a_file" }
400 { "error": "suffix_not_allowed:.txt" } / { "error": "file_too_large:..." } / { "error": "path_required" }
GET /health   → { ok, engines, root, max_bytes, timeout_s }
```

## Guards

| Guard | Value | Why |
|---|---|---|
| Path root | `TOOL_ROOT` (default `./`) | `..` and absolute paths outside the root are refused, after `resolve()` so symlinks cannot escape |
| Suffix allowlist | pdf, png, jpg, jpeg, tif, tiff, bmp, webp | an unexpected file type is a bug, not a feature |
| Max bytes | 15 MB (`MAX_BYTES`) | a hostile or accidental 500 MB upload cannot occupy a worker |
| Subprocess timeout | 20 s (`OCR_TIMEOUT`) | a hung engine becomes an error, not a stuck worker |
| Empty result | flagged `scanned: true` | a scan is reported as a scan; never returned as an empty success |

## Pipeline

1. `.pdf` → `pdftotext -layout` (poppler), fallback `pypdf`.
2. If the extracted text is under 40 chars per page, the file is treated as a scan and
   flagged. It is not silently returned as empty.
3. Images → `tesseract` when installed. On a host without tesseract the service answers
   `503 ocr_engine_missing` with the exact install hint, instead of pretending.

`GET /health` exists so the gateway can decide *before* routing a job whether this worker
can do it — a capability check, not an error at request time.

## Run

```bash
make test    # 7 tests: contract, scan detection, path escape, absolute path, 404, health, image gap
make run     # 127.0.0.1:8099
make demo    # three live calls against the samples
TOOL_ROOT=/path/to/pdfs make run   # widen the allowed root explicitly
```

## Deliberately missing

- Authentication. The gateway is the only caller; this binds `127.0.0.1`.
- Queueing and retries for large batches. A 200-page scan belongs in a job queue, not in
  the request path — the gateway is expected to hand those off asynchronously.
- Language auto-detection, deskewing, table reconstruction. Each is a separate feature with
  its own accuracy claims; this service reports what it actually did (`engine`), so those
  claims stay honest when they are added.

## Wiring into the gateway

The gateway calls this worker over HTTP and does not import it:

```bash
# worker
TOOL_ROOT=/path/to/files PORT=8098 python3 server.py
# gateway
OCR_SERVICE_URL=http://127.0.0.1:8098 python3 -m gateway.server
```

Live end-to-end (real file, real text):

```
$ python3 cli.py --chat dm "ocr CV-TranTrungDung-VI.pdf"
5792 ký tự, 2 trang, engine pdftotext. TRẦN TRUNG DŨNG / Backend Engineer ...

$ python3 cli.py --chat group_internal "ocr CV-TranTrungDung-VI.pdf"
"Sending that to you privately."   → content delivered by DM
```

The audit log records the difference, which is the point of the split:

```
{"user":"1001","chat":"dm","tool":"ocr_extract","decision":"answered"}
{"user":"1001","chat":"group_internal","tool":"ocr_extract","decision":"private_delivery"}
```
