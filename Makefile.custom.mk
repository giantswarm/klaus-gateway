# Repo-specific make targets. The generated Makefile.gen.*.mk are owned by devctl.

# Commit of giantswarm/kagent-upstream the kagent.api.v1alpha1 protos under
# hack/kagent-proto/ are copied from. Bump it, run `make generate-kagent`, and
# commit hack/kagent-proto/ and pkg/kagent/gen/ together.
KAGENT_PROTO_REPO ?= https://github.com/giantswarm/kagent-upstream.git
KAGENT_PROTO_COMMIT ?= 0ac524031db451634359df3081d7d5d08cea4c93
KAGENT_PROTO_FILES := common agent_instances agent_templates models system harnesses

.PHONY: generate-kagent
generate-kagent: ## Refresh the kagent protos from KAGENT_PROTO_COMMIT and regenerate pkg/kagent/gen.
	@tmp=$$(mktemp -d) && git clone -q --filter=blob:none --no-checkout $(KAGENT_PROTO_REPO) $$tmp \
	  && git -C $$tmp checkout -q $(KAGENT_PROTO_COMMIT) -- proto/kagent/api/v1alpha1 proto/buf.lock \
	  && for f in $(KAGENT_PROTO_FILES); do cp $$tmp/proto/kagent/api/v1alpha1/$$f.proto hack/kagent-proto/kagent/api/v1alpha1/; done \
	  && rm -rf $$tmp
	cd hack/kagent-proto && PATH="$$(go env GOPATH)/bin:$$PATH" buf generate
	# The repo's CI checks every Go file with goimports; protoc-gen-go groups
	# imports differently, so the generated files are formatted once here.
	go run golang.org/x/tools/cmd/goimports@v0.50.0 -local github.com/giantswarm/klaus-gateway -w pkg/kagent/gen
	sed -i 's/^KAGENT_PROTO_COMMIT: .*/KAGENT_PROTO_COMMIT: $(KAGENT_PROTO_COMMIT)/' pkg/kagent/gen/README.md

# The architect orb's go-build job links the release binary with the .ldflags
# file its go-test step writes, and runs `make test` (the job's test_target)
# between the two. The orb stamps pkg/project.gitSHA and buildTimestamp there,
# not version, and the Go build info cannot stand in: the module path has no /v3
# suffix, so the toolchain ignores the v2+ tags. This appends the version
# `make build` stamps, gitsemver's, which is also the image tag the orb pushes:
# the release on a tag build, a dev version on a branch. A no-op without
# .ldflags (local runs) and once the orb stamps the version itself.
.PHONY: ldflags-version
ldflags-version:
	@if [ -f .ldflags ] && [ -n "$(VERSION)" ] && ! grep -q '/pkg/project\.version=' .ldflags; then \
		printf " -X '%s/pkg/project.version=%s'" "$(MODULE)" "$(VERSION)" >> .ldflags; \
		echo "====> $@: $(VERSION)"; \
	fi

test: ldflags-version
