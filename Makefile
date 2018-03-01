BRANCH = "master"
VERSION = $(shell cat ./VERSION)
SEEDS ?= "127.0.0.1:20333"
PORT ?= "3000"

build:
	@go build -o ./bin/neo-go ./cli/main.go

check-version:
	git fetch && (! git rev-list ${VERSION})

deps:
	@dep ensure

push-tag:
	git checkout ${BRANCH}
	git pull origin ${BRANCH}
	git tag ${VERSION}
	git push origin ${BRANCH} --tags

run: build
	./bin/neo-go node -seed ${SEEDS} -tcp ${PORT}

test:
	@go test ./... -cover

vet:
	@go vet ./...
