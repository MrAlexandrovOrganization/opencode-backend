GO_VERSION := $(shell awk '$$1 == "go" {print $$2; exit}' go.mod)
ALPINE_VERSION := 3.21
VERSION_VARS := GO_VERSION ALPINE_VERSION
export $(VERSION_VARS)
