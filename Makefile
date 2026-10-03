.PHONY: opencode air

opencode:
	nono run --profile opencode-go --allow-cwd -- opencode

air:
	nono run --profile opencode-go --allow-cwd -- bash -c "source .env; air"
