.PHONY: check fmt vet lint generate test proto build install installctl push-dockerimage

generate:
	@echo "Generating mock files..."
	@bash scripts/generate.sh

fmt:
	@echo "Checking Go file formatting..."
	@bash scripts/checks/fmt.sh

vet:
	@echo "Checking for common Go mistakes..."
	@bash scripts/checks/vet.sh

lint:
	@echo "Checking for style errors..."
	@bash scripts/checks/lint.sh

check: generate fmt vet lint

test: generate
	@echo "Running tests..."
	@bash scripts/test.sh

proto:
	@echo "Generating proto code..."
	@bash scripts/genproto.sh

build:
	@echo "Compiling jackal binary..."
	@bash scripts/compile.sh

install:
	@echo "Installing jackal binary..."
	@bash scripts/install.sh

installctl:
	@echo "Installing jackalctl binary..."
	@bash scripts/installctl.sh

push-dockerimage:
	@echo "Pushing docker image..."
	@bash scripts/dockerimage.sh
