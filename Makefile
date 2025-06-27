

build-arm:
	GOOS=linux GOARCH=arm64 go build -o jetclock-updater-arm
