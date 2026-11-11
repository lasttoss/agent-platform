PY := python3

.PHONY: test test-gateway test-ocr run-gateway run-ocr run-adapter demo-adapter

test: test-gateway test-ocr

test-gateway:
	@echo "== gateway =="
	@cd services/gateway && $(PY) -m unittest discover -s tests

test-ocr:
	@echo "== ocr =="
	@cd services/ocr && $(PY) -m unittest discover -s tests

run-gateway:
	cd services/gateway && PORT=8088 $(PY) -m gateway.server

run-adapter:
	cd services/gateway && python3 adapters/telegram_poll.py

demo-adapter:
	cd services/gateway && python3 demo_adapter.py

run-ocr:
	cd services/ocr && PORT=8099 $(PY) server.py
