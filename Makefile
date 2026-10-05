# Makefile — criteria-adapter-decision
#
# Build/test plus the security & dependency-freshness tooling required by the
# Criteria supply-chain policy (mirrors the monorepo's WS49/WS50 and the
# criteria-adapter-noop pattern). See docs/dependency-policy.md for the rules
# these targets enforce.
#
# Tool versions are pinned here (no floating @latest) so CI and local runs
# resolve the SAME version — reproducibility and supply-chain safety. This
# single-module repo pins tools in the Makefile rather than a separate tools/
# go.mod (the monorepo's mechanism); bump these deliberately.

GO ?= go

OSV_SCANNER_VERSION     := v2.3.8
GO_MOD_OUTDATED_VERSION := v0.9.0
GOMAJOR_VERSION         := v0.15.0

.PHONY: help build test vet tidy manifest-check lint vuln-scan deps-outdated deps-majors

help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  %-16s %s\n", $$1, $$2}'

build: ## Build the adapter
	$(GO) build ./...

test: ## Run tests
	$(GO) test -race ./...

vet: ## go vet
	$(GO) vet ./...

# Manifest round-trip (ADR-0013 D7, KB-204): the SDK default --emit-manifest
# emits JSON that the host's strict parser must accept. The deep gates live
# in TestManifestRoundTrip (compiles + executes the binary too); this target
# is the CI-visible structural check: emit → strict jq gates → determinism.
manifest-check: ## Manifest round-trip: --emit-manifest JSON passes structural gates twice, byte-identical
	set -e; checkdir=$$(mktemp -d); trap 'rm -rf "$$checkdir"' EXIT; \
	$(GO) build -o "$$checkdir/criteria-adapter-decision" . && \
	"$$checkdir/criteria-adapter-decision" --emit-manifest > "$$checkdir/manifest.json" && \
	"$$checkdir/criteria-adapter-decision" --emit-manifest > "$$checkdir/manifest2.json" && \
	cmp "$$checkdir/manifest.json" "$$checkdir/manifest2.json" && \
	jq -e '.schema_version == 1 and .name == "decision" and (.version | test("^[0-9]+\\.[0-9]+\\.[0-9]+")) and .sdk_protocol_version == 2 and (.source_url | test("^https://")) and (.platforms | length) > 0 and (.capabilities | index("parallel_safe") != null) and (.secrets | length) == 1 and .secrets[0].name == "api_key" and (.config_schema.fields | length) == 5 and .config_schema.fields.base_url.required == true and .config_schema.fields.model.required == true and .config_schema.fields.retries.default == "0" and (.input_schema.fields | length) == 3 and .input_schema.fields.api_key.sensitive == true and (.output_schema.fields | length) == 3 and .output_schema.fields.answers.type == "array" and .output_schema.fields.usage.type == "object" and .output_schema.fields.error.type == "object"' \
		"$$checkdir/manifest.json" > /dev/null && \
	echo "manifest round-trip OK"

tidy: ## go mod tidy
	$(GO) mod tidy

# --- Security gate (WS49) -----------------------------------------------------

vuln-scan: ## Scan for known vulnerabilities (osv-scanner; local parity with CI osv-scan)
	$(GO) run github.com/google/osv-scanner/v2/cmd/osv-scanner@$(OSV_SCANNER_VERSION) scan source -r .

# --- Dependency freshness (WS50) ---------------------------------------------
# The source of truth for "are we on latest major.minor", not Dependabot.

deps-outdated: ## Report direct deps behind their latest minor/patch (go-mod-outdated)
	$(GO) list -u -m -json all | $(GO) run github.com/psampaz/go-mod-outdated@$(GO_MOD_OUTDATED_VERSION) -update -direct

deps-majors: ## List available major-version (/vN) upgrades (gomajor); apply per dependency-policy
	$(GO) run github.com/icholy/gomajor@$(GOMAJOR_VERSION) list