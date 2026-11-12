PY := python3

.PHONY: test samples test-gateway test-ocr test-model test-go parity \
	run-gateway run-gateway-go run-mcp-demo run-model run-ocr run-adapter demo-adapter

test: samples test-gateway test-ocr test-model test-go

samples:
	@python3 services/ocr/samples/make_samples.py >/dev/null && echo "== samples =="

test-gateway:
	@echo "== gateway =="
	@cd services/gateway && $(PY) -m unittest discover -s tests

test-ocr:
	@echo "== ocr =="
	@cd services/ocr && $(PY) -m unittest discover -s tests

test-model:
	@echo "== model-mcp =="
	@cd services/model-mcp && $(PY) -m unittest discover -s tests

test-go:
	@echo "== gateway-go =="
	@cd services/gateway-go && go test ./...

parity:
	@echo "== parity: python vs go =="
	@python3 scripts/parity.py

# The Go gateway is the gateway. Python's gateway/ service is kept as the reference the parity
# script compares against.
run-gateway-go:
	cd services/gateway-go && go run . -addr 127.0.0.1:8090 \
		-mcp ../../config/mcp.json -tools ../../config/tools.json

run-mcp-demo:
	cd services/gateway-go && go run ./cmd/mcp-demo -addr 127.0.0.1:8091

run-model:
	cd services/model-mcp && PORT=8097 $(PY) server.py

run-gateway:
	cd services/gateway && PORT=8088 $(PY) -m gateway.server

run-adapter:
	cd services/gateway && python3 adapters/telegram_poll.py

demo-adapter:
	cd services/gateway && python3 demo_adapter.py

run-ocr:
	cd services/ocr && PORT=8099 $(PY) server.py
