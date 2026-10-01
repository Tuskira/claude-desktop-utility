BIN := bin/interceptor

AGENT_LABEL  := com.tuskira.interceptor
AGENT_SRC    := deploy/macos/$(AGENT_LABEL).plist
AGENT_DST    := $(HOME)/Library/LaunchAgents/$(AGENT_LABEL).plist

.PHONY: build test vet fmt check clean install-agent uninstall-agent

build:
	@mkdir -p bin
	go build -o $(BIN) .

test:
	go test ./... -count=1

vet:
	go vet ./...

fmt:
	gofmt -l -w .

check: vet test
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)

clean:
	rm -rf bin

# install-agent renders deploy/macos/com.tuskira.interceptor.plist (a
# template: launchd does not expand $HOME) with this machine's home
# directory, this repo's path, and GATEWAY_URL, lints it, and installs it as
# a launchd LaunchAgent. Usage:
#   make install-agent GATEWAY_URL=https://your-gateway.example.com
# Safe to re-run: it bootstraps out any already-loaded copy first.
install-agent:
	@if [ -z "$(GATEWAY_URL)" ]; then \
		echo "usage: make install-agent GATEWAY_URL=https://your-gateway.example.com" >&2; \
		exit 1; \
	fi
	@mkdir -p "$(HOME)/Library/LaunchAgents"
	sed -e "s#__HOME__#$(HOME)#g" -e "s#__REPO__#$(CURDIR)#g" -e "s#__GATEWAY_URL__#$(GATEWAY_URL)#g" \
		$(AGENT_SRC) > $(AGENT_DST)
	plutil -lint $(AGENT_DST)
	-launchctl bootout gui/$$(id -u)/$(AGENT_LABEL) 2>/dev/null
	launchctl bootstrap gui/$$(id -u) $(AGENT_DST)
	@echo "installed and started $(AGENT_LABEL); logs: tail -f $(HOME)/Library/Logs/interceptor.log"

# uninstall-agent stops and removes the LaunchAgent installed by install-agent.
uninstall-agent:
	-launchctl bootout gui/$$(id -u)/$(AGENT_LABEL) 2>/dev/null
	rm -f $(AGENT_DST)
	@echo "removed $(AGENT_LABEL)"
