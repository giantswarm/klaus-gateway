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
	go run google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.12 --version >/dev/null
	cd hack/kagent-proto && PATH="$$(go env GOPATH)/bin:$$PATH" buf generate
	sed -i 's/^KAGENT_PROTO_COMMIT: .*/KAGENT_PROTO_COMMIT: $(KAGENT_PROTO_COMMIT)/' pkg/kagent/gen/README.md
