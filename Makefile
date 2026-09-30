PLUGIN_ID      := kandev-plugin-lambda-microvm
VERSION        := $(shell sed -n 's/^version: *"\(.*\)"/\1/p' manifest.yaml)
BUILD_DIR      := build
DIST           := dist
KANDEV_BACKEND ?= ../kandev/apps/backend
# npm packages to pre-install in the image, e.g. AGENTS="opencode-ai@1.18.32".
AGENTS         ?=
PLATFORMS      := linux-amd64 linux-arm64 darwin-amd64 darwin-arm64 windows-amd64

.PHONY: all check build-all build-image-artifacts package verify-live build-image teardown clean

all: check

check:
	go mod tidy -diff
	@test -z "$$(gofmt -l .)" || (gofmt -l .; exit 1)
	go vet ./...
	go test -race ./...

## Plugin binaries for every platform in manifest.yaml.
build-all:
	@mkdir -p $(BUILD_DIR)/package/server
	@for platform in $(PLATFORMS); do \
		os=$${platform%-*}; arch=$${platform#*-}; ext=""; [ "$$os" = windows ] && ext=.exe; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -o $(BUILD_DIR)/package/server/plugin-$$os-$$arch$$ext . || exit 1; \
	done

## The MicroVM image build context (build/image.zip): Dockerfile, bootstrap, agentctl
## for linux/arm64, and the agents to pre-install.
build-image-artifacts:
	@mkdir -p $(BUILD_DIR)/image/dist
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o $(BUILD_DIR)/image/dist/microvm-bootstrap ./bootstrap
	cd $(KANDEV_BACKEND) && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o $(CURDIR)/$(BUILD_DIR)/image/dist/agentctl ./cmd/agentctl
	cp bootstrap/Dockerfile.microvm $(BUILD_DIR)/image/Dockerfile
	printf '%s\n' $(AGENTS) > $(BUILD_DIR)/image/agents.txt
	cd $(BUILD_DIR)/image && rm -f ../image.zip && zip -qr ../image.zip .

## Installable package, built with Kandev's plugin-pack so it carries checksums.
package: build-all
	cp manifest.yaml $(BUILD_DIR)/package/
	@mkdir -p $(DIST)
	cd $(KANDEV_BACKEND) && go run ./cmd/plugin-pack -dir $(CURDIR)/$(BUILD_DIR)/package -out $(CURDIR)/$(DIST)/$(PLUGIN_ID)-$(VERSION).tar.gz

## Launch a real environment, drive agentctl through the leases, and tear it down.
verify-live:
	@test -n "$(REGION)" -a -n "$(IMAGE)" || (echo "usage: make verify-live REGION=... IMAGE=..."; exit 2)
	go run ./cmd/verify-live -region $(REGION) -image $(IMAGE)

build-image:
	@test -n "$(REGION)" -a -n "$(NAME)" -a -n "$(ROLE)" -a -n "$(ARTIFACT)" || (echo "usage: make build-image REGION=... NAME=... ROLE=... ARTIFACT=s3://..."; exit 2)
	go run ./cmd/build-image -region $(REGION) -name $(NAME) -role $(ROLE) -artifact $(ARTIFACT)

teardown:
	@test -n "$(REGION)" || (echo "usage: make teardown REGION=... [IMAGES='arn ...']"; exit 2)
	go run ./cmd/teardown $(REGION) $(IMAGES)

clean:
	rm -rf $(BUILD_DIR) $(DIST)
