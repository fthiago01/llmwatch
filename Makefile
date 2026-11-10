.PHONY: build test run install

build:
	go build -o llmwatch .

test:
	go test -race ./...

run:
	go run . serve

# instala a partir do código local, reaproveitando o serviço criado pelo install.sh
install:
	go build -o $(HOME)/.local/bin/llmwatch .
	systemctl --user restart llmwatch
