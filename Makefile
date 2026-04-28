GO ?= go
KUSTOMIZE ?= kustomize
HELM ?= helm
DOCKER ?= docker
GOWORK ?= off
IMAGE ?= ghcr.io/appthrust/aws-workload-identity-operator:dev
REMOTE_IRSA_TOOLS_IMAGE ?= ghcr.io/appthrust/aws-workload-identity-operator/remote-irsa-tools:dev
AWS_IRSA_SIDECAR_IMAGE ?= ghcr.io/appthrust/aws-workload-identity-operator/aws-irsa-sidecar:dev
CONTROLLER_GEN ?= $(GO) tool -modfile tools/go.mod controller-gen
GOLANGCI_LINT ?= $(GO) tool -modfile tools/go.mod golangci-lint

.PHONY: test
test:
	GOWORK=$(GOWORK) $(GO) test ./...

.PHONY: fmt
fmt:
	gofmt -w api cmd internal pkg

.PHONY: lint
lint:
	GOWORK=$(GOWORK) $(GOLANGCI_LINT) run ./...

.PHONY: generate
generate: generate-crds

.PHONY: generate-crds
generate-crds:
	GOWORK=$(GOWORK) $(CONTROLLER_GEN) crd paths=./api/... output:crd:dir=config/crd/bases
	cp config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml charts/aws-workload-identity-operator/templates/crds/aws.identity.appthrust.io_awsworkloadidentityconfigs.yaml
	cp config/crd/bases/aws.identity.appthrust.io_awsserviceaccountroles.yaml charts/aws-workload-identity-operator/templates/crds/aws.identity.appthrust.io_awsserviceaccountroles.yaml
	cp config/crd/bases/aws.identity.appthrust.io_awsserviceaccountrolereplicasets.yaml charts/aws-workload-identity-operator/templates/crds/aws.identity.appthrust.io_awsserviceaccountrolereplicasets.yaml
	cp config/crd/bases/aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml charts/aws-workload-identity-operator/templates/crds/aws.identity.appthrust.io_awsworkloadidentityoperatorconfigs.yaml

.PHONY: verify-generated
verify-generated: generate
	git diff --exit-code -- config/crd/bases charts/aws-workload-identity-operator/templates/crds

.PHONY: manifests
manifests: generate-crds
	$(KUSTOMIZE) build config/default

.PHONY: helm-lint
helm-lint:
	$(HELM) lint charts/aws-workload-identity-operator
	$(HELM) lint charts/aws-workload-identity-operator --values charts/aws-workload-identity-operator/values.test.yaml

.PHONY: docs-lint
docs-lint:
	./hack/verify-docs.sh

.PHONY: verify-version-sync
verify-version-sync:
	./hack/sync-chart-version.sh --check

.PHONY: helm-template
helm-template:
	@$(HELM) template aws-workload-identity-operator charts/aws-workload-identity-operator \
		--namespace aws-workload-identity-operator-system \
		--hide-notes

.PHONY: helm-template-test
helm-template-test:
	@$(HELM) template aws-workload-identity-operator charts/aws-workload-identity-operator \
		--namespace aws-workload-identity-operator-system \
		--values charts/aws-workload-identity-operator/values.test.yaml \
		--hide-notes

.PHONY: docker-build
docker-build:
	$(DOCKER) buildx build -t $(IMAGE) --load .

.PHONY: build-remote-irsa-tools
build-remote-irsa-tools:
	mkdir -p bin
	GOWORK=$(GOWORK) CGO_ENABLED=0 $(GO) build -o bin/aws-remote-irsa-credential-process ./cmd/aws-remote-irsa-credential-process
	GOWORK=$(GOWORK) CGO_ENABLED=0 $(GO) build -o bin/aws-irsa-sidecar ./cmd/aws-irsa-sidecar

.PHONY: docker-build-remote-irsa-tools
docker-build-remote-irsa-tools:
	$(DOCKER) buildx build --target remote-irsa-tools -t $(REMOTE_IRSA_TOOLS_IMAGE) --load .

.PHONY: build-aws-irsa-sidecar
build-aws-irsa-sidecar:
	mkdir -p bin
	GOWORK=$(GOWORK) CGO_ENABLED=0 $(GO) build -o bin/aws-irsa-sidecar ./cmd/aws-irsa-sidecar

.PHONY: docker-build-aws-irsa-sidecar
docker-build-aws-irsa-sidecar:
	$(DOCKER) buildx build --target aws-irsa-sidecar -t $(AWS_IRSA_SIDECAR_IMAGE) --load .

.PHONY: test-kest
test-kest:
	bun test test/kest

.PHONY: e2e-selfhosted-irsa
e2e-selfhosted-irsa:
	./hack/e2e-selfhosted-irsa.sh

.PHONY: e2e-remote-irsa-consumer
e2e-remote-irsa-consumer:
	./hack/e2e-remote-irsa-consumer.sh

.PHONY: verify-static
verify-static:
	! rg -n 'kro.run|RESTConfigFromKubeConfig|sigs.k8s.io/cluster-api|(^|[^[:alnum:]])cluster\.x-k8s\.io' api cmd internal pkg config --glob '*.go' --glob '*.yaml'
