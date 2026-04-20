# DO NOT EDIT. Generated with:
#
#    devctl
#
#    https://github.com/giantswarm/devctl/blob/c2dd604fd787d9aa63ec6c43c817c8596f1356f7/pkg/gen/input/makefile/internal/file/Makefile.template
#

include Makefile.*.mk

##@ Helm

.PHONY: helm-test
helm-test: ## Run helm lint + render assertions for the klaus-gateway chart.
	@echo "====> $@"
	./hack/helm-template-tests

##@ E2E

E2E_COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: e2e-local e2e-local-up e2e-local-down
e2e-local: e2e-local-up ## Bring up the compose smoke stack and run hack/smoke-completion.
	@echo "====> $@"
	./hack/wait-for http://127.0.0.1:8081/healthz 120
	./hack/smoke-completion http://127.0.0.1:8080 test-instance
	$(MAKE) e2e-local-down

e2e-local-up: ## Bring the compose stack up in the background.
	@echo "====> $@"
	$(E2E_COMPOSE) up -d --build --wait --wait-timeout 120

e2e-local-down: ## Tear the compose stack down and remove volumes.
	@echo "====> $@"
	-$(E2E_COMPOSE) down -v --remove-orphans

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk commands is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z%\\\/_0-9-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)
