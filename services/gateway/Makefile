PY := python3

.PHONY: run test demo policy-check

run:
	$(PY) -m gateway.server

test:
	$(PY) -m unittest discover -s tests -v

demo:
	@echo "--- 1. DM: personal answer in chat"
	@$(PY) cli.py --chat dm "lương tháng này của tôi bao nhiêu"
	@echo "--- 2. group_internal: same question, answer routed to DM"
	@$(PY) cli.py --chat group_internal "lương tháng này của tôi bao nhiêu"
	@echo "--- 3. group_public: no personal tool visible at all"
	@$(PY) cli.py --chat group_public "lương tháng này của tôi bao nhiêu"
	@echo "--- 4. unknown user"
	@$(PY) cli.py --user 9999 "lương của tôi"

policy-check:
	$(PY) -c "from gateway import policy; r=policy.load_rules('config/policy.json'); print('policy ok:', list(r['chats']))"
