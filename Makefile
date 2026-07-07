# webchat Makefile. The embedded chat.js is minified via vite before
# go:embed picks it up, so consumers of the Go library don't need a JS
# toolchain — web/dist/chat.js is committed. Rebuild and commit it whenever
# web/src/chat.js changes.

.PHONY: assets assets-install clean test

# Install JS dev dependencies (esbuild).
assets-install:
	npm install

# Minify web/src/chat.js -> web/dist/chat.js.
assets:
	npm run build

# Build everything an embedding host needs: install deps then minify.
build-assets: assets-install assets

# Run Go tests.
test:
	go test ./...

# Remove the built bundle (the source-of-truth web/src/chat.js is preserved).
clean:
	rm -rf web/dist
