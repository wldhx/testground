GOTFLATS ?=
SHELL = /bin/bash

define eachmod
	@echo '$(1)'
	@find . -type f -name go.mod -print0 | xargs -I '{}' -n1 -0 bash -c 'dir="$$(dirname {})" && echo "$${dir}" && cd "$${dir}" && $(1)'
endef

.PHONY: install tidy mod-download lint build-all docker install test

install: goinstall docker

goinstall:
	go install .

pre-commit:
	python -m pip install pre-commit --upgrade --user
	pre-commit install --install-hooks

tidy:
	$(call eachmod,go mod tidy)

mod-download:
	$(call eachmod,go mod download)

lint:
	$(call eachmod,GOGC=75 golangci-lint run --concurrency 32 --deadline 4m ./...)

build-all:
	$(call eachmod,go build -o /dev/null ./...)

docker: docker-testground docker-sidecar

docker-sidecar:
	docker build -t iptestground/sidecar:edge -f Dockerfile.sidecar .

docker-testground:
	docker build -t iptestground/testground:edge -f Dockerfile.testground .

test: install
	testground plan import --from ./plans/placebo || true
	testground plan import --from ./plans/example || true
	$(call eachmod,go test -p 1 -v $(GOTFLAGS) ./...)
