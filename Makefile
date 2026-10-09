.PHONY: web web-embed build-webui build-webui-arm64 build-webui-arm build-webui-mipsle build-bot build-bot-arm64 build-bot-arm build-bot-mipsle build-watchd build-watchd-arm64 build-watchd-arm build-watchd-mipsle build-all clean

# Build Vue SPA
web:
	cd web && npm ci && npm run build

# Copy dist into Go embed location
web-embed: web
	rm -rf server/cmd/webui/web/dist
	mkdir -p server/cmd/webui/web
	cp -r web/dist server/cmd/webui/web/dist

# Build webui binary (requires web-embed first)
build-webui: web-embed
	make -C server build-webui

build-webui-arm64: web-embed
	make -C server build-webui-arm64

build-webui-arm: web-embed
	make -C server build-webui-arm

build-webui-mipsle: web-embed
	make -C server build-webui-mipsle

# Build bot (unchanged)
build-bot:
	make -C server build

build-bot-arm64:
	make -C server build-arm64

build-bot-arm:
	make -C server build-arm

build-bot-mipsle:
	make -C server build-mipsle

# Build the server monitor (no SPA to embed)
build-watchd:
	make -C server build-watchd

build-watchd-arm64:
	make -C server build-watchd-arm64

build-watchd-arm:
	make -C server build-watchd-arm

build-watchd-mipsle:
	make -C server build-watchd-mipsle

# Build all
build-all: build-webui-arm64 build-webui-arm build-webui-mipsle build-bot-arm64 build-bot-arm build-bot-mipsle build-watchd-arm64 build-watchd-arm build-watchd-mipsle

clean:
	rm -rf web/dist server/cmd/webui/web/dist
	make -C server clean
