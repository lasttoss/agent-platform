PY := python3

.PHONY: test samples test-gateway test-ocr test-go parity run-gateway run-ocr run-adapter demo-adapter

test: samples test-gateway test-ocr test-go

samples:
	@python3 services/ocr/samples/make_samples.py >/dev/null && echo "== samples =="

test-gateway:
	@echo "== gateway =="
	@cd services/gateway && $(PY) -m unittest discover -s tests

test-ocr:
	@echo "== ocr =="
	@cd services/ocr && $(PY) -m unittest discover -s tests

test-go:
	@echo "== gateway-go =="
	@cd services/gateway-go && go test ./...

parity:
	@echo "== parity: python vs go =="
	@python3 scripts/parity.py

run-gateway:
	cd services/gateway && PORT=8088 $(PY) -m gateway.server

run-adapter:
	cd services/gateway && python3 adapters/telegram_poll.py

demo-adapter:
	cd services/gateway && python3 demo_adapter.py

run-ocr:
	cd services/ocr && PORT=8099 $(PY) server.py
