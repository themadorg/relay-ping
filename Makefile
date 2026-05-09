APP_NAME := relay-ping
BIN_DIR := bin
CMD_DIR := ./cmd/relay-ping
WEBXDC_FILE := $(BIN_DIR)/$(APP_NAME).xdc
WEBXDC_WITH_RESULTS := $(BIN_DIR)/$(APP_NAME)-with-results.xdc

.PHONY: build build-webxdc build-webxdc-with-results clean

build:
	mkdir -p $(BIN_DIR)
	go build -o $(BIN_DIR)/$(APP_NAME) $(CMD_DIR)

build-webxdc:
	mkdir -p $(BIN_DIR)
	sed -i 's/WEBXDC_BUILD_MODE = false/WEBXDC_BUILD_MODE = true/' web/static/app.js
	cd web && zip -r ../$(WEBXDC_FILE) manifest.toml index.html static
	sed -i 's/WEBXDC_BUILD_MODE = true/WEBXDC_BUILD_MODE = false/' web/static/app.js

# Bundle a latency_matrix export for offline WebXDC (CI). Requires RUN_EXPORT=path/to/run.json.gz
build-webxdc-with-results:
ifndef RUN_EXPORT
	$(error RUN_EXPORT must point to a latency matrix .json.gz export)
endif
	mkdir -p $(BIN_DIR)
	cp $(RUN_EXPORT) web/static/bundled-run.json.gz
	sed -i 's/WEBXDC_BUILD_MODE = false/WEBXDC_BUILD_MODE = true/' web/static/app.js
	cd web && zip -r ../$(WEBXDC_WITH_RESULTS) manifest.toml index.html static
	sed -i 's/WEBXDC_BUILD_MODE = true/WEBXDC_BUILD_MODE = false/' web/static/app.js
	rm -f web/static/bundled-run.json.gz

clean:
	rm -rf $(BIN_DIR)
