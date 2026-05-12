# xerotty build targets.
#
#   make            — build the bare ./xerotty binary (same as build.sh)
#   make app        — assemble xerotty.app (macOS bundle) for proper
#                     Dock-icon coalescing and Cmd+H / dock menu support
#   make install    — copy xerotty.app into /Applications (macOS)
#   make clean      — remove built artifacts

BINARY      := xerotty
APP_NAME    := xerotty
APP_BUNDLE  := $(APP_NAME).app
BUNDLE_ID   := cc.xeron.xerotty
VERSION     := 0.1.0
BUILD_NUM   := 1
GO          := go

UNAME_S := $(shell uname -s)

.PHONY: all build app install clean

all: build

build:
	$(GO) build -o $(BINARY) ./cmd/xerotty
	@echo "built: $(CURDIR)/$(BINARY)"

# Assemble a macOS .app bundle. The Info.plist's CFBundleIdentifier is
# what tells Cocoa to coalesce multiple running processes of the same
# bundle under a single Dock icon (with a windows menu under it),
# instead of each `exec.Command(os.Executable())` "new window" call
# getting its own Dock entry.
app: build
ifneq ($(UNAME_S),Darwin)
	@echo "make app: skipping — not on macOS (uname says $(UNAME_S))"
else
	rm -rf $(APP_BUNDLE)
	mkdir -p $(APP_BUNDLE)/Contents/MacOS
	mkdir -p $(APP_BUNDLE)/Contents/Resources
	cp -f $(BINARY) $(APP_BUNDLE)/Contents/MacOS/$(BINARY)
	@printf '%s\n' \
		'<?xml version="1.0" encoding="UTF-8"?>' \
		'<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">' \
		'<plist version="1.0">' \
		'<dict>' \
		'    <key>CFBundleDevelopmentRegion</key><string>en</string>' \
		'    <key>CFBundleExecutable</key><string>$(BINARY)</string>' \
		'    <key>CFBundleIdentifier</key><string>$(BUNDLE_ID)</string>' \
		'    <key>CFBundleInfoDictionaryVersion</key><string>6.0</string>' \
		'    <key>CFBundleName</key><string>$(APP_NAME)</string>' \
		'    <key>CFBundleDisplayName</key><string>$(APP_NAME)</string>' \
		'    <key>CFBundlePackageType</key><string>APPL</string>' \
		'    <key>CFBundleShortVersionString</key><string>$(VERSION)</string>' \
		'    <key>CFBundleVersion</key><string>$(BUILD_NUM)</string>' \
		'    <key>CFBundleSignature</key><string>????</string>' \
		'    <key>LSMinimumSystemVersion</key><string>11.0</string>' \
		'    <key>NSHighResolutionCapable</key><true/>' \
		'    <key>NSPrincipalClass</key><string>NSApplication</string>' \
		'</dict>' \
		'</plist>' \
		> $(APP_BUNDLE)/Contents/Info.plist
	@echo "bundled: $(CURDIR)/$(APP_BUNDLE)"
endif

install: app
ifneq ($(UNAME_S),Darwin)
	@echo "make install: skipping — not on macOS"
else
	rm -rf /Applications/$(APP_BUNDLE)
	cp -Rf $(APP_BUNDLE) /Applications/
	@echo "installed: /Applications/$(APP_BUNDLE)"
endif

clean:
	rm -f $(BINARY)
	rm -rf $(APP_BUNDLE)
