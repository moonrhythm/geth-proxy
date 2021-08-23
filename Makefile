COMMIT_SHA=$(shell git rev-parse HEAD)

docker:
	mbuild build \
		--frontend dockerfile.v0 \
		--local dockerfile=. \
		--local context=. \
		--output type=image,name=gcr.io/moonrhythm-containers/geth-proxy:$(COMMIT_SHA),push=true
