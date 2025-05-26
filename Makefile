dev-build:
	go build -o ./dist/cue-dev ./cmd/cue

install:
	go install ./cmd/cue
