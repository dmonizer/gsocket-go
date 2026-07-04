.PHONY: all build test clean windows linux darwin

APP_NAME := gs-netcat
LDFLAGS := -ldflags="-s -w"

build:
	go build $(LDFLAGS) ./cmd/$(APP_NAME)

test:
	go test -v -count=1 ./gsocket/...

vet:
	go vet ./...

windows:
	GOOS=windows GOARCH=amd64 go build $(LDFLAGS) -o $(APP_NAME).exe ./cmd/$(APP_NAME)

linux:
	GOOS=linux GOARCH=amd64 go build $(LDFLAGS) -o $(APP_NAME)-linux ./cmd/$(APP_NAME)

darwin:
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(APP_NAME)-darwin ./cmd/$(APP_NAME)

all: windows linux darwin
	@echo "Built for all platforms."

clean:
	rm -f $(APP_NAME) $(APP_NAME).exe $(APP_NAME)-linux $(APP_NAME)-darwin
