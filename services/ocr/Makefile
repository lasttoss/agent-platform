PY := python3

.PHONY: run test demo health

run:
	$(PY) server.py

test:
	$(PY) -m unittest discover -s tests -v

health:
	@curl -s localhost:8099/health | $(PY) -m json.tool

demo:
	@echo "--- text PDF ---"
	@curl -s -X POST localhost:8099/v1/ocr -H 'Content-Type: application/json' \
	  -d '{"path":"samples/text.pdf"}' | $(PY) -m json.tool | head -20
	@echo "--- scanned PDF (no text layer) ---"
	@curl -s -X POST localhost:8099/v1/ocr -H 'Content-Type: application/json' \
	  -d '{"path":"samples/scan.pdf"}' | $(PY) -m json.tool
	@echo "--- path escape attempt ---"
	@curl -s -X POST localhost:8099/v1/ocr -H 'Content-Type: application/json' \
	  -d '{"path":"/etc/hostname"}' | $(PY) -m json.tool
