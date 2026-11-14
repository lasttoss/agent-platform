PY := python3

.PHONY: test samples test-gateway test-ocr test-model test-userdir test-clickhouse test-audit test-go parity \
	run-gateway run-gateway-go run-ts-example run-mcp-demo run-model run-ocr run-userdir \
	run-audit-sink
	run-clickhouse run-adapter demo-adapter check-users compose-up compose-down compose-logs compose-smoke

test: samples test-gateway test-ocr test-model test-userdir test-clickhouse test-audit test-go

samples:
	@python3 services/ocr/samples/make_samples.py >/dev/null && echo "== samples =="

test-gateway:
	@echo "== gateway =="
	@cd services/gateway && $(PY) -m unittest discover -s tests

test-userdir:
	@echo "== userdir =="
	@cd services/userdir && $(PY) -m unittest discover -s tests

test-audit:
	@echo "== audit-sink =="
	@cd services/audit-sink && $(PY) -m unittest discover -s tests

test-clickhouse:
	@echo "== clickhouse =="
	@cd services/clickhouse && $(PY) -m unittest discover -s tests

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

run-ts-example:
	cd services/mcp-ts-example && PORT=8095 node --experimental-strip-types server.ts

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

run-clickhouse:
	cd services/clickhouse && PORT=8098 CLICKHOUSE_URL=$${CLICKHOUSE_URL:-http://127.0.0.1:8124} $(PY) server.py

run-userdir:
	cd services/userdir && PORT=8093 $(PY) server.py

run-audit-sink:
	cd services/audit-sink && PORT=8096 $(PY) server.py

# Compares the user directory with policy.json and exits non-zero on a mismatch.
check-users:
	cd services/gateway-go && go run . -check-users -refresh 0

# The whole platform as separate processes, with Postgres, one command.
compose-up:
	docker compose -f deploy/compose/docker-compose.yml up --build -d
	@echo "gateway: http://127.0.0.1:8090/v1/whoami?user=1001&chat=dm"

compose-down:
	docker compose -f deploy/compose/docker-compose.yml down -v

compose-smoke:
	bash scripts/compose_smoke.sh

compose-logs:
	docker compose -f deploy/compose/docker-compose.yml logs -f --tail=50
