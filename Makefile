.PHONY: all opencode air

all:
	alacritty --working-directory=$(CURDIR) -e bash -c "make opencode; exec bash" &
	$(MAKE) air

opencode:
	nono run --profile opencode-go --allow-cwd -- opencode

air:
	nono run --profile opencode-go --allow-cwd -- bash -c "source .env; air"
