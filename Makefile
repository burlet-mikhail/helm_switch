.PHONY: build app dmg clean install

BINARY_NAME = HelmSwitch
APP_NAME    = Helm Switch.app
DMG_NAME    = HelmSwitch.dmg
BUILD_DIR   = build
APP_DIR     = $(BUILD_DIR)/$(APP_NAME)
RESOURCES   = $(APP_DIR)/Contents/Resources
MACOS_DIR   = $(APP_DIR)/Contents/MacOS
ICONSET     = /tmp/HelmSwitch.iconset
LSREGISTER  = /System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister
VERSION     = 1.0.0

# --------------------------------------------------------------------------
build:
	@echo "Building $(BINARY_NAME)..."
	go build -ldflags="-s -w" -o "$(BUILD_DIR)/$(BINARY_NAME)" .
	@echo "Binary: $(BUILD_DIR)/$(BINARY_NAME)"

# --------------------------------------------------------------------------
app: build icon
	@echo "Creating .app bundle..."
	@mkdir -p "$(MACOS_DIR)" "$(RESOURCES)"
	@cp "$(BUILD_DIR)/$(BINARY_NAME)" "$(MACOS_DIR)/$(BINARY_NAME)"
	@chmod +x "$(MACOS_DIR)/$(BINARY_NAME)"
	@cp packaging/Info.plist "$(APP_DIR)/Contents/Info.plist"
	@echo "  ✔  $(APP_DIR)"

icon:
	@echo "Generating icon..."
	@python3 scripts/gen_icon.py
	@mkdir -p $(ICONSET)
	@for size in 16 32 64 128 256 512; do \
		sips -z $$size $$size /tmp/ruswitch_icon_src.png \
			--out $(ICONSET)/icon_$${size}x$${size}.png > /dev/null; \
		double=$$((size * 2)); \
		sips -z $$double $$double /tmp/ruswitch_icon_src.png \
			--out $(ICONSET)/icon_$${size}x$${size}@2x.png > /dev/null; \
	done
	@mkdir -p "$(RESOURCES)"
	@iconutil -c icns $(ICONSET) -o "$(RESOURCES)/AppIcon.icns"
	@echo "  ✔  $(RESOURCES)/AppIcon.icns"

# --------------------------------------------------------------------------
dmg: app
	@echo "Creating DMG..."
	@rm -f "$(BUILD_DIR)/$(DMG_NAME)"
	@# Create a temp dir for DMG contents
	@rm -rf /tmp/helmswitch_dmg
	@mkdir /tmp/helmswitch_dmg
	@cp -r "$(APP_DIR)" /tmp/helmswitch_dmg/
	@ln -s /Applications /tmp/helmswitch_dmg/Applications
	@hdiutil create \
		-volname "Helm Switch $(VERSION)" \
		-srcfolder /tmp/helmswitch_dmg \
		-ov -format UDZO \
		"$(BUILD_DIR)/$(DMG_NAME)"
	@rm -rf /tmp/helmswitch_dmg
	@echo "  ✔  $(BUILD_DIR)/$(DMG_NAME)"

# --------------------------------------------------------------------------
install: app
	@echo "Installing to ~/Applications/..."
	@# Stop any running instance (old or new executable name).
	@killall RuSwitch HelmSwitch 2>/dev/null || true
	@# Remove the previous install (both the old name and this one).
	@rm -rf ~/Applications/RuSwitch.app
	@rm -rf ~/Applications/"$(APP_NAME)"
	@cp -r "$(APP_DIR)" ~/Applications/"$(APP_NAME)"
	@# Force LaunchServices/Spotlight to pick up the new name and icon.
	@$(LSREGISTER) -f ~/Applications/"$(APP_NAME)" 2>/dev/null || true
	@echo "  ✔  ~/Applications/$(APP_NAME)"
	@echo "  →  Re-grant Accessibility for 'Helm Switch' and launch it."

# --------------------------------------------------------------------------
build-windows:
	@echo "Building $(BINARY_NAME) for Windows..."
	GOOS=windows GOARCH=amd64 CGO_ENABLED=1 CC=x86_64-w64-mingw32-gcc \
		go build -ldflags="-s -w -H windowsgui" -o "$(BUILD_DIR)/$(BINARY_NAME).exe" .
	@echo "  ✔  $(BUILD_DIR)/$(BINARY_NAME).exe"

# --------------------------------------------------------------------------
release: dmg build-windows
	@echo "Release artifacts:"
	@ls -lh "$(BUILD_DIR)/$(DMG_NAME)" "$(BUILD_DIR)/$(BINARY_NAME).exe"

# --------------------------------------------------------------------------
clean:
	@echo "Cleaning..."
	@rm -rf "$(BUILD_DIR)/$(BINARY_NAME)" "$(APP_DIR)" "$(BUILD_DIR)/$(DMG_NAME)"
	@echo "  ✔  clean"
