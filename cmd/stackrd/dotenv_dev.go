//go:build !release

package main

// A source build (hamr dev, make build) reads ./.env; the release image is built
// with -tags release and takes its settings from the environment only.
import _ "github.com/joho/godotenv/autoload"
