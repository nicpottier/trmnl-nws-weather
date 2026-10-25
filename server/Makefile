# `sam build` calls build-GlanceFunction; `make zip` builds a bundle for uploading by hand.
GOFLAGS_LAMBDA = GOOS=linux GOARCH=arm64 CGO_ENABLED=0

.PHONY: build-GlanceFunction zip test run deploy

build-GlanceFunction:
	$(GOFLAGS_LAMBDA) go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o $(ARTIFACTS_DIR)/bootstrap .

zip:
	mkdir -p dist
	$(GOFLAGS_LAMBDA) go build -tags lambda.norpc -trimpath -ldflags="-s -w" -o dist/bootstrap .
	cd dist && rm -f glance.zip && zip -q glance.zip bootstrap

test:
	go test ./...

run:
	go run .

deploy:
	sam build && sam deploy --guided
